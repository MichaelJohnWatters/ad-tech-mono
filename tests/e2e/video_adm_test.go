//go:build e2e

// OpenRTB §4.3/§4.4 VAST-in-adm end-to-end: the internal DSP now bids VAST
// XML in bid.adm (Protocol 13), the exchange substitutes ${AUCTION_PRICE} at
// win time, the SSP forwards adm gated on the imp's advertised protocols, and
// the publisher-adserver serves the BUYER's document with platform-signed
// trackers injected — falling back to the legacy MediaURL build when the
// protocol gate drops the adm. The external flow is proven with a FakeDSP
// returning its own VAST whose impression carries the §4.4 price macro.
package e2e

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/vast"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func fetchVAST(t *testing.T, h *harness.Harness, placementExternal string) (*vast.VAST, []byte) {
	t.Helper()
	var doc vast.VAST
	var raw []byte
	harness.WaitFor(t, 20*time.Second, "video auction fills "+placementExternal, func() bool {
		reqURL := h.URLs.PublisherAdServer + routes.PublisherAdServeVAST + "?placement_id=" + placementExternal
		req, _ := http.NewRequest(http.MethodGet, reqURL, nil)
		resp, err := h.HTTP.Do(req)
		if err != nil {
			return false
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return false
		}
		doc = vast.VAST{}
		if err := xml.Unmarshal(body, &doc); err != nil {
			return false
		}
		if len(doc.Ads) == 0 || doc.Ads[0].InLine == nil {
			return false
		}
		raw = body
		return true
	})
	return &doc, raw
}

// TestVideoAdMInternalDSP: the standard path for our own demand — the served
// VAST is the DSP's adm document (AdSystem ad-tech-mono-dsp) with the
// platform's signed trackers injected, and the beacons actually land.
func TestVideoAdMInternalDSP(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.Reset(t)

	uniq := fmt.Sprintf("vadm-%d", time.Now().UnixNano())
	pubAcc := h.CreatePublisher(t, "vadm-pub-"+uniq)
	pub := h.AddPublisher(t, pubAcc, "vadm-pub-"+uniq, "vadm-"+uniq+".test")
	h.AddVideoPlacement(t, pub, "vadm-pl-"+uniq, 1.00, 6, 30)
	adv := h.CreateAdvertiser(t, "adv-acme")
	h.GrantBalance(t, adv.ID, 100_000, "vadm-grant-"+uniq)
	vio := h.CreateInsertionOrder(t, adv, "vadm-io-"+uniq, 5000)
	h.CreateVideoCampaign(t, adv, vio, "vadm-li-"+uniq, 10.0, 500,
		"vadm-cr-"+uniq, "ford-"+uniq+".test", 15, harness.Targeting{})
	h.RefreshAllCaches(t)

	doc, _ := fetchVAST(t, h, "vadm-pl-"+uniq)
	in := doc.Ads[0].InLine
	if in.AdSystem.Name != "ad-tech-mono-dsp" {
		t.Fatalf("served ad must come from the DSP's adm (AdSystem ad-tech-mono-dsp), got %q — adm path not taken", in.AdSystem.Name)
	}
	// Platform-injected trackers: signed impression + quartiles + error macro.
	if len(in.Impressions) == 0 || !strings.Contains(in.Impressions[0].URI, routes.TrackerImpression) {
		t.Fatalf("platform impression not injected: %+v", in.Impressions)
	}
	if len(in.Errors) == 0 || !strings.Contains(in.Errors[0].URI, "ec=[ERRORCODE]") {
		t.Errorf("platform <Error> with ec=[ERRORCODE] not injected: %+v", in.Errors)
	}
	var imp, start string
	imp = strings.TrimSpace(in.Impressions[0].URI)
	for _, cr := range in.Creatives.Creatives {
		if cr.Linear == nil || cr.Linear.TrackingEvents == nil {
			continue
		}
		for _, tr := range cr.Linear.TrackingEvents.Tracking {
			if tr.Event == "start" && strings.Contains(tr.URI, routes.TrackerVideo) {
				start = strings.TrimSpace(tr.URI)
			}
		}
	}
	if start == "" {
		t.Fatal("injected start quartile (/v1/t/video) missing")
	}

	// Fire the injected beacons like a player — they must land in analytics.
	tid := queryParam(t, imp, "tid")
	fireBeacon(t, h, vast.ExpandURIMacros(imp, vast.URIMacroValues{}))
	fireBeacon(t, h, vast.ExpandURIMacros(start, vast.URIMacroValues{}))
	harness.WaitFor(t, 15*time.Second, "injected start beacon recorded", func() bool {
		return h.MediaEventsByTrace(t, tid, "video", "start", "") >= 1
	})
}

// TestVideoAdMExternalBidder: a third-party buyer's VAST-in-adm survives the
// whole chain — its own impression beacon is preserved WITH the §4.4
// ${AUCTION_PRICE} macro substituted by the exchange (clearing price, not a
// literal), and the platform's signed trackers ride alongside.
func TestVideoAdMExternalBidder(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.Reset(t)

	uniq := fmt.Sprintf("vext-%d", time.Now().UnixNano())
	pubAcc := h.CreatePublisher(t, "vext-pub-"+uniq)
	pub := h.AddPublisher(t, pubAcc, "vext-pub-"+uniq, "vext-"+uniq+".test")
	h.AddVideoPlacement(t, pub, "vext-pl-"+uniq, 1.00, 6, 30)
	h.RefreshAllCaches(t)

	// External buyer: VAST 4.2 in adm, Protocol 13, buyer impression carrying
	// the §4.4 macros. High price so it wins the fan-out outright.
	buyerVAST := `<VAST version="4.2"><Ad id="ext-e2e"><InLine>` +
		`<AdSystem>fake-ext-dsp</AdSystem><AdTitle>FakeExt</AdTitle>` +
		`<Impression><![CDATA[https://buyer.example/imp?p=${AUCTION_PRICE}&a=${AUCTION_ID}]]></Impression>` +
		`<Creatives><Creative><Linear><Duration>00:00:15</Duration>` +
		`<MediaFiles><MediaFile delivery="progressive" type="video/mp4" width="640" height="360">` +
		`<![CDATA[https://cdn.buyer.example/v.mp4]]></MediaFile></MediaFiles>` +
		`</Linear></Creative></Creatives></InLine></Ad></VAST>`
	fake := harness.NewFakeDSP(t, harness.FakeDSPOpts{
		Mode: harness.FakeDSPBidder, BidPrice: 40.0, Seat: "fake-ext-seat",
		BidMutate: func(bid *openrtb.BidObj, req openrtb.BidRequest) {
			bid.AdM = buyerVAST
			bid.Protocol = openrtb.ProtocolVAST42
			bid.W, bid.H, bid.Dur = 640, 360, 15
		},
	})
	h.SetConfigForPod(t, "exchange.dsp_endpoints", fake.URL, harness.PodExchange)
	t.Cleanup(func() {
		h.SetConfigForPod(t, "exchange.dsp_endpoints",
			h.URLs.ClusterDSP+","+h.URLs.ClusterDSPComp1+","+h.URLs.ClusterDSPComp2, harness.PodExchange)
	})

	doc, raw := fetchVAST(t, h, "vext-pl-"+uniq)
	in := doc.Ads[0].InLine
	if in.AdSystem.Name != "fake-ext-dsp" {
		t.Fatalf("served ad must be the external buyer's document, got AdSystem %q", in.AdSystem.Name)
	}
	if strings.Contains(string(raw), "${AUCTION_PRICE}") || strings.Contains(string(raw), "${AUCTION_ID}") {
		t.Fatalf("§4.4 auction macros left unsubstituted in the served document:\n%s", raw)
	}
	var buyerImp string
	var sawPlatformImp bool
	for _, i := range in.Impressions {
		u := strings.TrimSpace(i.URI)
		if strings.Contains(u, "buyer.example/imp") {
			buyerImp = u
		}
		if strings.Contains(u, routes.TrackerImpression) {
			sawPlatformImp = true
		}
	}
	if buyerImp == "" || !sawPlatformImp {
		t.Fatalf("want buyer + platform impressions, got %+v", in.Impressions)
	}
	// The substituted price must be a positive number (the clearing price).
	price, err := strconv.ParseFloat(queryParam(t, buyerImp, "p"), 64)
	if err != nil || price <= 0 {
		t.Errorf("buyer impression p= must carry the substituted clearing price, got %q (err=%v)", buyerImp, err)
	}
	// Buyer's external CDN MediaFile untouched by re-hosting.
	if !strings.Contains(string(raw), "cdn.buyer.example/v.mp4") {
		t.Errorf("buyer CDN media URL must survive untouched:\n%s", raw)
	}
}

// TestVideoAdMProtocolGateFallback: a placement that only accepts VAST 2.0
// (protocols [2]) makes the SSP drop the DSP's 4.2 adm — and the serve falls
// back to the locally-built document (AdSystem ad-tech-mono). Proves the
// protocol gate AND the fallback wiring live, in one pass.
func TestVideoAdMProtocolGateFallback(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.Reset(t)

	uniq := fmt.Sprintf("vgate-%d", time.Now().UnixNano())
	pubAcc := h.CreatePublisher(t, "vgate-pub-"+uniq)
	pub := h.AddPublisher(t, pubAcc, "vgate-pub-"+uniq, "vgate-"+uniq+".test")
	h.AddVideoPlacementProtocols(t, pub, "vgate-pl-"+uniq, 1.00, 6, 30, []int{2})
	adv := h.CreateAdvertiser(t, "adv-acme")
	h.GrantBalance(t, adv.ID, 100_000, "vgate-grant-"+uniq)
	vio := h.CreateInsertionOrder(t, adv, "vgate-io-"+uniq, 5000)
	h.CreateVideoCampaign(t, adv, vio, "vgate-li-"+uniq, 10.0, 500,
		"vgate-cr-"+uniq, "ford-"+uniq+".test", 15, harness.Targeting{})
	h.RefreshAllCaches(t)

	doc, _ := fetchVAST(t, h, "vgate-pl-"+uniq)
	in := doc.Ads[0].InLine
	if in.AdSystem.Name != "ad-tech-mono" {
		t.Fatalf("protocol-gated winner must serve via the local MediaURL build (AdSystem ad-tech-mono), got %q", in.AdSystem.Name)
	}
	if len(in.Impressions) == 0 || !strings.Contains(in.Impressions[0].URI, routes.TrackerImpression) {
		t.Errorf("fallback build must still carry the signed impression: %+v", in.Impressions)
	}
}

// TestVideoAdMPod: pod slots ride the adm path too — each sequenced ad is the
// DSP's document with platform trackers injected per slot.
func TestVideoAdMPod(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.Reset(t)

	uniq := fmt.Sprintf("vpod-%d", time.Now().UnixNano())
	pubAcc := h.CreatePublisher(t, "vpod-pub-"+uniq)
	pub := h.AddPublisher(t, pubAcc, "vpod-pub-"+uniq, "vpod-"+uniq+".test")
	h.AddVideoPlacement(t, pub, "vpod-pl-"+uniq, 1.00, 6, 30)
	adv := h.CreateAdvertiser(t, "adv-acme")
	h.GrantBalance(t, adv.ID, 100_000, "vpod-grant-"+uniq)
	vio := h.CreateInsertionOrder(t, adv, "vpod-io-"+uniq, 5000)
	h.CreateVideoCampaign(t, adv, vio, "vpod-li-"+uniq, 10.0, 500,
		"vpod-cr-"+uniq, "ford-"+uniq+".test", 15, harness.Targeting{})
	h.RefreshAllCaches(t)

	var doc vast.VAST
	harness.WaitFor(t, 20*time.Second, "pod fills", func() bool {
		reqURL := h.URLs.PublisherAdServer + routes.PublisherAdServeVAST + "?pod=3&placement_id=vpod-pl-" + uniq
		req, _ := http.NewRequest(http.MethodGet, reqURL, nil)
		resp, err := h.HTTP.Do(req)
		if err != nil {
			return false
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		doc = vast.VAST{}
		return resp.StatusCode == http.StatusOK && xml.Unmarshal(body, &doc) == nil && len(doc.Ads) >= 1
	})
	for i, ad := range doc.Ads {
		if ad.Sequence != i+1 {
			t.Errorf("pod ad %d sequence = %d, want %d", i, ad.Sequence, i+1)
		}
		if ad.InLine == nil || ad.InLine.AdSystem.Name != "ad-tech-mono-dsp" {
			t.Errorf("pod slot %d must ride the adm path, got %+v", i, ad.InLine)
		}
		if len(ad.InLine.Impressions) == 0 {
			t.Errorf("pod slot %d missing injected impression", i)
		}
	}
}

func queryParam(t *testing.T, rawURL, key string) string {
	t.Helper()
	i := strings.Index(rawURL, key+"=")
	if i < 0 {
		t.Fatalf("url %q missing param %s", rawURL, key)
	}
	v := rawURL[i+len(key)+1:]
	if j := strings.IndexByte(v, '&'); j >= 0 {
		v = v[:j]
	}
	return v
}

func fireBeacon(t *testing.T, h *harness.Harness, u string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		t.Fatalf("beacon request %q: %v", u, err)
	}
	browserHeaders(req)
	resp, err := h.HTTP.Do(req)
	if err != nil {
		t.Fatalf("fire beacon: %v", err)
	}
	resp.Body.Close()
}
