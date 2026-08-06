//go:build e2e

package harness

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// AuctionResult is the parsed response from the SSP /v1/ssp/request endpoint.
// The SSP wraps the exchange's OpenRTB BidResponse with placement context.
type AuctionResult struct {
	TraceID     string          `json:"trace_id"`
	PlacementID string          `json:"placement_id"`
	PublisherID string          `json:"publisher_id"`
	SiteDomain  string          `json:"site_domain"`
	Width       int             `json:"width"`
	Height      int             `json:"height"`
	BidResponse json.RawMessage `json:"bid_response"`
}

// AuctionParams carries the optional bid-signal query params the SSP stamps
// into the OpenRTB request. Empty fields are omitted.
type AuctionParams struct {
	Placement  string // friendly external key, not the UUID
	Channel    string // serve channel (video/audio/native/dooh/retail/ingame); empty = display
	Categories string // retail: the shopper's browsed IAB categories (?cat=) → Site.Cat
	Surfaces   int    // in-game intrinsic: number of scene surfaces (?surfaces=) → imp.ext.surfaces
	Geo        string // ISO country (Device.geo.country)
	Device     string // device type keyword
	UserID     string // User.id (drives segment lookup)
	UID2       string // Unified ID 2.0 token → User.eids (cookieless identity)
	OS         string // Device.os — for OS targeting
	IP         string // client IP (?ip=) — drives the SSP's household derivation
	Keywords   string // Site.keywords (comma-separated) — for keyword targeting
	Segments   string // User.ext.segments (comma-separated) — for segment targeting
	// Privacy signals the SSP stamps into Regs / User.ext.consent. Let privacy
	// tests exercise the consent path end-to-end (SSP → exchange → DSP).
	GDPR      string // "1" sets Regs.ext.gdpr
	Consent   string // TCF consent string → User.ext.consent
	USPrivacy string // IAB US Privacy string → Regs.ext.us_privacy
	COPPA     string // "1" sets Regs.coppa
	GPP       string // GPP string → Regs.ext.gpp
	GPPSID    string // GPP section ids → Regs.ext.gpp_sid
	GPC       string // "1" sets Regs.ext.gpc (Global Privacy Control)
}

// RunAuction drives an auction through SSP → Exchange → DSPs as if a real
// publisher page requested an ad. Returns the parsed response so subtests
// can assert on winning DSP, clearing price, deal_id.
//
// placementExternalID is the friendly YAML/test key (e.g. "pl-news-mpu"),
// not the UUID — the SSP service does the derivation server-side.
func (h *Harness) RunAuction(t *testing.T, placementExternalID, geo, device, userID string) AuctionResult {
	return h.RunAuctionWith(t, AuctionParams{Placement: placementExternalID, Geo: geo, Device: device, UserID: userID})
}

// RunAuctionWith is RunAuction with the full optional bid-signal set (OS,
// keywords). Existing callers use RunAuction; targeting tests use this.
func (h *Harness) RunAuctionWith(t *testing.T, p AuctionParams) AuctionResult {
	t.Helper()

	vals := neturl.Values{}
	add := func(k, v string) {
		if v != "" {
			vals.Set(k, v)
		}
	}
	add("placement_id", p.Placement)
	add("channel", p.Channel)
	add("cat", p.Categories)
	if p.Surfaces > 0 {
		add("surfaces", strconv.Itoa(p.Surfaces))
	}
	add("geo", p.Geo)
	add("device", p.Device)
	add("user_id", p.UserID)
	add("uid2", p.UID2)
	add("os", p.OS)
	add("ip", p.IP)
	add("keywords", p.Keywords)
	add("segments", p.Segments)
	add("gdpr", p.GDPR)
	add("gdpr_consent", p.Consent)
	add("us_privacy", p.USPrivacy)
	add("coppa", p.COPPA)
	add("gpp", p.GPP)
	add("gpp_sid", p.GPPSID)
	add("gpc", p.GPC)

	url := h.URLs.SSP + "/v1/ssp/request"
	if len(vals) > 0 {
		url += "?" + vals.Encode()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("auction request: %v", err)
	}
	resp, err := h.HTTP.Do(req)
	if err != nil {
		t.Fatalf("auction call failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("auction status %d: %s", resp.StatusCode, string(body))
	}
	var out AuctionResult
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("auction decode: %v\nbody: %s", err, string(body))
	}
	return out
}

// PostOpenRTBBid POSTs a raw OpenRTB bid-request JSON straight to a service's
// /v1/openrtb/bid endpoint, bypassing the SSP/exchange. Enforcement tests use it
// to control the EXACT inbound request — e.g. an unsigned ads.cert request — and
// then assert on the returned NBR. Because the DSP's ads.cert gate runs before
// any targeting, a block is provable (NBR set) regardless of whether a campaign
// would otherwise have matched, so no biddable-world setup is needed for the
// reject case. baseURL is a host-reachable DSP URL (e.g. h.URLs.DSP).
func (h *Harness) PostOpenRTBBid(t *testing.T, baseURL, bodyJSON string) BidResponseWinner {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+routes.OpenRTBBid, strings.NewReader(bodyJSON))
	if err != nil {
		t.Fatalf("build raw bid request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.HTTP.Do(req)
	if err != nil {
		t.Fatalf("raw bid call failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("raw bid status %d: %s", resp.StatusCode, string(body))
	}
	return h.ExtractWinner(t, AuctionResult{BidResponse: body})
}

// SSPServeResult is the browser-visible slice of the SSP /v1/ssp/serve
// response: rendered HTML (or NoBid) with no auction internals.
type SSPServeResult struct {
	TraceID string `json:"trace_id"`
	NoBid   bool   `json:"nobid,omitempty"`
	HTML    string `json:"html,omitempty"`
	// Slots is the retail sponsored-results grid (one entry per filled slot).
	Slots []struct {
		Position      int     `json:"position"`
		CampaignID    string  `json:"campaign_id"`
		ClearingPrice float64 `json:"clearing_price"`
		ImpressionID  string  `json:"impression_id"`
	} `json:"slots,omitempty"`
}

// ServeViaSSP drives the realistic visitor path (SSP runs the auction AND
// calls the ad server to render) with the same signal set as RunAuctionWith.
// Freq-cap blocks surface as NoBid here — the SSP maps the ad server's 429
// to the standard nobid response.
func (h *Harness) ServeViaSSP(t *testing.T, p AuctionParams) SSPServeResult {
	t.Helper()

	vals := neturl.Values{}
	add := func(k, v string) {
		if v != "" {
			vals.Set(k, v)
		}
	}
	add("placement_id", p.Placement)
	add("channel", p.Channel)
	add("cat", p.Categories)
	if p.Surfaces > 0 {
		add("surfaces", strconv.Itoa(p.Surfaces))
	}
	add("geo", p.Geo)
	add("device", p.Device)
	add("user_id", p.UserID)
	add("uid2", p.UID2)
	add("os", p.OS)
	add("ip", p.IP)
	add("keywords", p.Keywords)
	add("segments", p.Segments)
	add("gdpr", p.GDPR)
	add("gdpr_consent", p.Consent)
	add("us_privacy", p.USPrivacy)
	add("coppa", p.COPPA)
	add("gpp", p.GPP)
	add("gpp_sid", p.GPPSID)
	add("gpc", p.GPC)

	url := h.URLs.SSP + routes.SSPServe
	if len(vals) > 0 {
		url += "?" + vals.Encode()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("ssp serve request: %v", err)
	}
	resp, err := h.HTTP.Do(req)
	if err != nil {
		t.Fatalf("ssp serve call failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ssp serve status %d: %s", resp.StatusCode, string(body))
	}
	var out SSPServeResult
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("ssp serve decode: %v\nbody: %s", err, string(body))
	}
	return out
}

// BidResponseWinner extracts the winning bid from a SSP-wrapped BidResponse.
// Returns the seat (advertiser/account UUID), clearing price, deal_id, and
// whether NoBid was set. Helper because the JSON shape is verbose to assert
// inline in every test step.
type BidResponseWinner struct {
	NoBid      bool
	Seat       string
	Price      float64
	CampaignID string
	CreativeID string
	DealID     string
	// ImpressionID is the OpenRTB BidObj.ID. For multi-winner auctions (in-game
	// surfaces, retail slots) this is the per-surface sub-trace the renderer fires
	// the impression on, so each surface bills independently.
	ImpressionID string
	// NBR is the OpenRTB no-bid reason code (0 = genuine no-bid / no demand;
	// ≥500 = an enforcement gate blocked the request — see pkg/openrtb). Lets a
	// test assert WHY a request no-bid without inferring it from a bare nobid.
	NBR       int
	NBRReason string
}

// ExtractWinner parses a BidResponse from an AuctionResult and pulls the
// fields tests usually care about. Doesn't fail the test on NoBid — the
// caller decides whether NoBid is expected for the scenario. On a no-bid it
// still surfaces NBR/NBRReason so the caller can distinguish an enforcement
// block from an ordinary "no demand".
func (h *Harness) ExtractWinner(t *testing.T, r AuctionResult) BidResponseWinner {
	t.Helper()
	var br struct {
		ID        string `json:"id"`
		NoBid     bool   `json:"nobid,omitempty"`
		NBR       int    `json:"nbr,omitempty"`
		NBRReason string `json:"nbrreason,omitempty"`
		SeatBid   []struct {
			Seat string `json:"seat"`
			Bid  []struct {
				ID     string  `json:"id"`
				Price  float64 `json:"price"`
				CID    string  `json:"cid"`
				CrID   string  `json:"crid"`
				DealID string  `json:"dealid,omitempty"`
			} `json:"bid"`
		} `json:"seatbid"`
	}
	if err := json.Unmarshal(r.BidResponse, &br); err != nil {
		t.Fatalf("decode bid response: %v\nraw: %s", err, string(r.BidResponse))
	}
	if br.NoBid || len(br.SeatBid) == 0 || len(br.SeatBid[0].Bid) == 0 {
		return BidResponseWinner{NoBid: true, NBR: br.NBR, NBRReason: br.NBRReason}
	}
	sb := br.SeatBid[0]
	b := sb.Bid[0]
	return BidResponseWinner{
		Seat: sb.Seat, Price: b.Price,
		CampaignID: b.CID, CreativeID: b.CrID, DealID: b.DealID,
		NBR: br.NBR, NBRReason: br.NBRReason,
	}
}

// ExtractAllWinners flattens EVERY bid across all SeatBids in a BidResponse — for
// multi-winner auctions (in-game scene surfaces, retail slate) where the response
// carries one SeatBid per winning advertiser. Order is response order (position 1
// first). Empty on a no-bid.
func (h *Harness) ExtractAllWinners(t *testing.T, r AuctionResult) []BidResponseWinner {
	t.Helper()
	var br struct {
		NoBid   bool `json:"nobid,omitempty"`
		SeatBid []struct {
			Seat string `json:"seat"`
			Bid  []struct {
				ID    string  `json:"id"`
				Price float64 `json:"price"`
				CID   string  `json:"cid"`
				CrID  string  `json:"crid"`
			} `json:"bid"`
		} `json:"seatbid"`
	}
	if err := json.Unmarshal(r.BidResponse, &br); err != nil {
		t.Fatalf("decode bid response: %v\nraw: %s", err, string(r.BidResponse))
	}
	var out []BidResponseWinner
	for _, sb := range br.SeatBid {
		for _, b := range sb.Bid {
			out = append(out, BidResponseWinner{Seat: sb.Seat, Price: b.Price, CampaignID: b.CID, CreativeID: b.CrID, ImpressionID: b.ID})
		}
	}
	return out
}
