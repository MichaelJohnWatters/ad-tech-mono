// cmd/extbidder is a standalone EXTERNAL DSP bidder — a simulated competitor
// that runs OUTSIDE the ad-tech cluster and speaks only OpenRTB, exactly as a
// real third-party DSP would. The in-cluster exchange fans out bid requests to
// it over the network (add its URL to exchange.dsp_endpoints), so it exercises
// the real OUTBOUND path: cross-network OpenRTB, the auction timeout budget, and
// a bidder the platform doesn't control.
//
// It is fully self-contained — no Postgres/Redis/NATS, no shared state. It just
// answers /v1/openrtb/bid with a renderable creative above the floor.
//
//	Run on the host:   go run ./cmd/extbidder            # :9100
//	Point the exchange at it (from inside the cluster):
//	  exchange.dsp_endpoints += http://host.docker.internal:9100
//
// Config (env): EXTBIDDER_PORT, EXTBIDDER_SEAT, EXTBIDDER_BRAND,
//
//	EXTBIDDER_MARKUP (bid = floor × (1+markup)), EXTBIDDER_NOBID_RATE,
//	EXTBIDDER_ADOMAIN, EXTBIDDER_VIDEO_URL, EXTBIDDER_AUDIO_URL,
//	EXTBIDDER_AUDIENCE_UPLIFT (extra markup when the request carries
//	standard-taxonomy audience data in user.data — a real buyer pays more
//	for inventory it can match to its audience models).
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

type bidder struct {
	seat           string
	brand          string
	markup         float64 // bid = floor * (1 + markup)
	audienceUplift float64 // extra markup when user.data carries segtax segments
	noBidRate      float64
	adomain        string
	videoURL       string
	audioURL       string
	rng            *rand.Rand

	// seen is a small ring of the user.data payloads received per bid
	// request — how e2e proves standard-taxonomy audience data actually
	// crossed the network boundary to an external buyer.
	seenMu sync.Mutex
	seen   []seenUserData
}

// seenUserData records what one bid request carried in user.data.
type seenUserData struct {
	RequestID string         `json:"request_id"`
	Data      []openrtb.Data `json:"data"`
}

const seenRingCap = 100

func (b *bidder) recordUserData(reqID string, data []openrtb.Data) {
	b.seenMu.Lock()
	defer b.seenMu.Unlock()
	b.seen = append(b.seen, seenUserData{RequestID: reqID, Data: data})
	if len(b.seen) > seenRingCap {
		b.seen = b.seen[len(b.seen)-seenRingCap:]
	}
}

// handleDebugUserData returns the recorded user.data payloads, newest last;
// ?id= filters to one bid request id.
func (b *bidder) handleDebugUserData(w http.ResponseWriter, r *http.Request) {
	b.seenMu.Lock()
	defer b.seenMu.Unlock()
	out := b.seen
	if id := r.URL.Query().Get("id"); id != "" {
		out = nil
		for _, s := range b.seen {
			if s.RequestID == id {
				out = append(out, s)
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if out == nil {
		out = []seenUserData{}
	}
	_ = json.NewEncoder(w).Encode(out)
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envF(k string, def float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func main() {
	b := &bidder{
		seat:           env("EXTBIDDER_SEAT", "ext-partner-dsp"),
		brand:          env("EXTBIDDER_BRAND", "Partner DSP"),
		markup:         envF("EXTBIDDER_MARKUP", 0.35),
		audienceUplift: envF("EXTBIDDER_AUDIENCE_UPLIFT", 0.10),
		noBidRate:      envF("EXTBIDDER_NOBID_RATE", 0.2),
		adomain:        env("EXTBIDDER_ADOMAIN", "partner-dsp.example"),
		videoURL:       env("EXTBIDDER_VIDEO_URL", "https://commondatastorage.googleapis.com/gtv-videos-bucket/sample/ForBiggerJoyrides.mp4"),
		audioURL:       env("EXTBIDDER_AUDIO_URL", "https://www.soundhelix.com/examples/mp3/SoundHelix-Song-1.mp3"),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	port := env("EXTBIDDER_PORT", "9100")

	mux := http.NewServeMux()
	mux.HandleFunc(routes.OpenRTBBid, b.handleBid)
	mux.HandleFunc("/v1/debug/user-data", b.handleDebugUserData)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })

	log.Printf("extbidder (external DSP) on :%s  seat=%s markup=%.0f%% nobid=%.0f%%",
		port, b.seat, b.markup*100, b.noBidRate*100)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

func (b *bidder) handleBid(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req openrtb.BidRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	// Standard-taxonomy audience data (user.data + ext.segtax) is the only
	// audience signal an external buyer can interpret — platform-internal
	// segment ids in user.ext mean nothing to us. Record it (even on a
	// no-bid, so the debug endpoint reflects everything received) and count
	// interpretable segments for the bid uplift.
	taxSegs := 0
	if req.User != nil && len(req.User.Data) > 0 {
		b.recordUserData(req.ID, req.User.Data)
		for _, d := range req.User.Data {
			if d.Ext != nil && d.Ext.Segtax > 0 {
				taxSegs += len(d.Segment)
			}
		}
		log.Printf("request %s: user.data with %d taxonomy segment(s)", req.ID, taxSegs)
	}

	nobid := func() { _ = json.NewEncoder(w).Encode(openrtb.BidResponse{ID: req.ID, NoBid: true}) }

	if len(req.Imp) == 0 || b.rng.Float64() < b.noBidRate {
		nobid()
		return
	}
	imp := req.Imp[0]
	floor := imp.BidFloor
	if floor <= 0 {
		floor = 1.0 // assume a $1 CPM floor when none is declared
	}
	price := floor * (1 + b.markup)
	if taxSegs > 0 {
		price *= 1 + b.audienceUplift
	}

	bid := openrtb.BidObj{
		ID:       "ext-" + req.ID,
		ImpID:    imp.ID,
		Price:    price,
		CrID:     "ext-creative",
		ADomain:  []string{b.adomain},
		BidModel: "cpm",
		DealID:   imp.DealID, // echo a PMP deal if the request carried one
	}

	switch {
	case imp.Video != nil:
		w, h := dim(imp.Video.W, imp.Video.H, 640, 360)
		bid.W, bid.H, bid.Dur = w, h, 15
		bid.AdM = b.vast(req.ID, w, h)
		bid.MediaURL = b.videoURL
		bid.Protocol = openrtb.ProtocolVAST42
	case imp.Audio != nil:
		bid.Dur = 15
		bid.AdM = b.vastAudio(req.ID)
		bid.MediaURL = b.audioURL
		bid.Protocol = openrtb.ProtocolVAST42
	case imp.Native != nil:
		bid.AdM = b.native()
	default: // banner (or unknown → serve a banner)
		bw, bh := 300, 250
		if imp.Banner != nil {
			bw, bh = dim(imp.Banner.W, imp.Banner.H, 300, 250)
		}
		bid.W, bid.H = bw, bh
		bid.AdM = b.banner(bw, bh, price)
	}

	resp := openrtb.BidResponse{
		ID:      req.ID,
		Cur:     "USD",
		SeatBid: []openrtb.SeatBid{{Seat: b.seat, Bid: []openrtb.BidObj{bid}}},
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func dim(w, h, dw, dh int) (int, int) {
	if w > 0 && h > 0 {
		return w, h
	}
	return dw, dh
}

// banner is a self-contained clickable creative, clearly branded as coming from
// the EXTERNAL partner DSP so a won impression is visibly attributable in a demo.
func (b *bidder) banner(w, h int, price float64) string {
	return fmt.Sprintf(`<div style="width:%dpx;height:%dpx;box-sizing:border-box;`+
		`background:linear-gradient(135deg,#0f172a,#4338ca);color:#fff;`+
		`font-family:system-ui,sans-serif;display:flex;flex-direction:column;`+
		`align-items:center;justify-content:center;text-align:center;cursor:pointer;border-radius:8px"`+
		` onclick="window.open('https://%s','_blank')">`+
		`<div style="font-size:11px;letter-spacing:2px;opacity:.7">EXTERNAL DSP</div>`+
		`<div style="font-size:22px;font-weight:700;margin:6px 0">%s</div>`+
		`<div style="font-size:12px;opacity:.85">won this auction @ $%.2f CPM</div>`+
		`<div style="margin-top:10px;background:#22c55e;color:#06210f;font-size:12px;`+
		`font-weight:600;padding:6px 14px;border-radius:6px">Learn more →</div></div>`,
		w, h, b.adomain, b.brand, price)
}

// vast builds the standard VAST 4.2 bid.adm. The Impression URI carries the
// OpenRTB §4.4 ${AUCTION_PRICE}/${AUCTION_ID} macros — the buyer's own
// delivery tracking, substituted by the EXCHANGE at win time with the
// clearing price (this is how a real DSP learns what it paid).
func (b *bidder) vast(auctionID string, w, h int) string {
	return `<VAST version="4.2"><Ad id="ext-` + auctionID + `"><InLine>` +
		`<AdSystem>extbidder</AdSystem><AdTitle>` + b.brand + ` (external DSP)</AdTitle>` +
		`<Impression><![CDATA[https://` + b.adomain + `/imp?p=${AUCTION_PRICE}&a=${AUCTION_ID}]]></Impression>` +
		`<Creatives><Creative><UniversalAdId idRegistry="extbidder">ext-creative</UniversalAdId>` +
		`<Linear><Duration>00:00:15</Duration>` +
		`<MediaFiles><MediaFile delivery="progressive" type="video/mp4" width="` +
		strconv.Itoa(w) + `" height="` + strconv.Itoa(h) + `"><![CDATA[` + b.videoURL +
		`]]></MediaFile></MediaFiles></Linear></Creative></Creatives></InLine></Ad></VAST>`
}

// vastAudio replaced the DAAST document 2026-10-05: DAAST was folded into
// VAST 4.1 by the IAB — modern audio is a VAST 4.x audio MediaFile. The SSP
// still ADVERTISES protocols 9/10 for hypothetical DAAST-only third parties,
// but nothing in this repo emits DAAST anymore.
func (b *bidder) vastAudio(auctionID string) string {
	return `<VAST version="4.2"><Ad id="ext-` + auctionID + `"><InLine>` +
		`<AdSystem>extbidder</AdSystem><AdTitle>` + b.brand + ` (external DSP)</AdTitle>` +
		`<Impression><![CDATA[https://` + b.adomain + `/imp?p=${AUCTION_PRICE}&a=${AUCTION_ID}]]></Impression>` +
		`<Creatives><Creative><UniversalAdId idRegistry="extbidder">ext-creative</UniversalAdId>` +
		`<Linear><Duration>00:00:15</Duration>` +
		`<MediaFiles><MediaFile delivery="progressive" type="audio/mpeg"><![CDATA[` +
		b.audioURL + `]]></MediaFile></MediaFiles></Linear></Creative></Creatives></InLine></Ad></VAST>`
}

func (b *bidder) native() string {
	// OpenRTB Native 1.2 response (stringified) — minimal title/data/link.
	esc := func(s string) string { return strings.ReplaceAll(s, `"`, `\"`) }
	return `{"native":{"assets":[` +
		`{"id":1,"title":{"text":"` + esc(b.brand+" — external DSP") + `"}},` +
		`{"id":2,"data":{"value":"Sponsored by the external partner DSP"}}],` +
		`"link":{"url":"https://` + b.adomain + `"}}}`
}
