//go:build e2e

// VAST 4.2 Wrapper chaining end-to-end (§3.19–3.23): a third-party-tag
// creative makes the DSP bid a Wrapper (protocol 14); the platform injects
// its signed trackers at WRAPPER level; the player (this test) follows
// VASTAdTagURI to the wrapped inline document and fires both hops' beacons —
// zero slippage across the chain. A tag that yields no ad records the spec's
// 303 through the wrapper <Error> with [ERRORCODE] substituted.
package e2e

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/vast"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestVideoWrapperChain(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.Reset(t)

	uniq := fmt.Sprintf("vwrap-%d", time.Now().UnixNano())
	pubAcc := h.CreatePublisher(t, "vwrap-pub-"+uniq)
	pub := h.AddPublisher(t, pubAcc, "vwrap-pub-"+uniq, "vwrap-"+uniq+".test")
	// Placement A = the wrapper buy; placement B = the "third party" supply
	// the tag resolves to (our own VAST endpoint — a live, always-up tag).
	h.AddVideoPlacement(t, pub, "vwrap-a-"+uniq, 1.00, 6, 30)
	h.AddVideoPlacement(t, pub, "vwrap-b-"+uniq, 1.00, 6, 30)

	adv := h.CreateAdvertiser(t, "adv-acme")
	h.GrantBalance(t, adv.ID, 100_000, "vwrap-grant-"+uniq)
	vio := h.CreateInsertionOrder(t, adv, "vwrap-io-"+uniq, 5000)
	// Geo-split so the two campaigns never collide: the wrapper campaign
	// wins A (dev-default geo USA); the inline campaign wins B (the tag URL
	// carries geo=GBR).
	tagURL := h.URLs.PublisherAdServer + routes.PublisherAdServeVAST + "?placement_id=vwrap-b-" + uniq + "&geo=GBR"
	h.CreateVideoWrapperCampaign(t, adv, vio, "vwrap-li-a-"+uniq, 12.0, 500,
		"vwrap-cr-a-"+uniq, "wrapbuy-"+uniq+".test", tagURL, 15, harness.Targeting{Geos: []string{"USA"}})
	h.CreateVideoCampaign(t, adv, vio, "vwrap-li-b-"+uniq, 8.0, 500,
		"vwrap-cr-b-"+uniq, "inline-"+uniq+".test", 15, harness.Targeting{Geos: []string{"GBR"}})
	h.RefreshAllCaches(t)

	fetchDoc := func(t *testing.T, rawURL string) (*vast.VAST, []byte) {
		t.Helper()
		var doc vast.VAST
		var raw []byte
		harness.WaitFor(t, 20*time.Second, "VAST fills: "+rawURL, func() bool {
			req, _ := http.NewRequest(http.MethodGet, rawURL, nil)
			resp, err := h.HTTP.Do(req)
			if err != nil {
				return false
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			doc = vast.VAST{}
			if resp.StatusCode != http.StatusOK || xml.Unmarshal(body, &doc) != nil || len(doc.Ads) == 0 {
				return false
			}
			raw = body
			return true
		})
		return &doc, raw
	}

	aURL := h.URLs.PublisherAdServer + routes.PublisherAdServeVAST + "?placement_id=vwrap-a-" + uniq

	// --- 1. A's document is a Wrapper with our signed wrapper-level trackers.
	doc, raw := fetchDoc(t, aURL)
	w := doc.Ads[0].Wrapper
	if w == nil {
		t.Fatalf("placement A must serve a <Wrapper>, got:\n%s", raw)
	}
	if got := strings.TrimSpace(w.VASTAdTagURI.URI); got != tagURL {
		t.Errorf("VASTAdTagURI = %q, want %q", got, tagURL)
	}
	if w.FollowAdditionalWrappers != "true" || w.AllowMultipleAds != "false" || w.FallbackOnNoAd != "true" {
		t.Errorf("wrapper behaviour attrs wrong: %+v", w)
	}
	if len(w.Impressions) == 0 || !strings.Contains(w.Impressions[0].URI, routes.TrackerImpression) {
		t.Fatalf("platform impression not injected at wrapper level: %+v", w.Impressions)
	}
	if len(w.Errors) == 0 || !strings.Contains(w.Errors[0].URI, "ec=[ERRORCODE]") {
		t.Fatalf("platform <Error> with ec=[ERRORCODE] not injected at wrapper level: %+v", w.Errors)
	}
	var wrapStart string
	if w.Creatives != nil {
		for _, cr := range w.Creatives.Creatives {
			if cr.Linear != nil && cr.Linear.TrackingEvents != nil {
				for _, tr := range cr.Linear.TrackingEvents.Tracking {
					if tr.Event == "start" {
						wrapStart = strings.TrimSpace(tr.URI)
					}
				}
			}
		}
	}
	if wrapStart == "" {
		t.Fatal("wrapper-level start tracking not injected")
	}
	wrapImp := strings.TrimSpace(w.Impressions[0].URI)
	tidA := queryParam(t, wrapImp, "tid")

	// --- 2. Act as the player across the hop: fire A's wrapper beacons,
	// follow the tag to B's inline document, fire B's beacons — events land
	// for BOTH traces (zero slippage across the chain).
	fireBeacon(t, h, vast.ExpandURIMacros(wrapImp, vast.URIMacroValues{}))
	fireBeacon(t, h, vast.ExpandURIMacros(wrapStart, vast.URIMacroValues{}))

	inner, innerRaw := fetchDoc(t, strings.ReplaceAll(tagURL, "[", "%5B"))
	in := inner.Ads[0].InLine
	if in == nil {
		t.Fatalf("tag must resolve to an InLine document:\n%s", innerRaw)
	}
	if in.AdSystem.Name != "ad-tech-mono-dsp" {
		t.Errorf("wrapped inline should ride the adm path, got AdSystem %q", in.AdSystem.Name)
	}
	var inImp, inStart string
	inImp = strings.TrimSpace(in.Impressions[0].URI)
	for _, cr := range in.Creatives.Creatives {
		if cr.Linear != nil && cr.Linear.TrackingEvents != nil {
			for _, tr := range cr.Linear.TrackingEvents.Tracking {
				if tr.Event == "start" && strings.Contains(tr.URI, routes.TrackerVideo) {
					inStart = strings.TrimSpace(tr.URI)
				}
			}
		}
	}
	if inStart == "" {
		t.Fatal("wrapped inline missing start tracking")
	}
	tidB := queryParam(t, inImp, "tid")
	if tidB == tidA {
		t.Fatalf("hop traces must differ (A=%s B=%s)", tidA, tidB)
	}
	fireBeacon(t, h, vast.ExpandURIMacros(inImp, vast.URIMacroValues{}))
	fireBeacon(t, h, vast.ExpandURIMacros(inStart, vast.URIMacroValues{}))

	harness.WaitFor(t, 15*time.Second, "both hops' start events recorded", func() bool {
		return h.MediaEventsByTrace(t, tidA, "video", "start", "") >= 1 &&
			h.MediaEventsByTrace(t, tidB, "video", "start", "") >= 1
	})

	// --- 3. Negative: a chain that yields no ad records 303 through the
	// wrapper's error beacon (fresh fetch = fresh trace; the player
	// substitutes [ERRORCODE]=303 per spec).
	t.Run("no_ad_after_wrapper_records_303", func(t *testing.T) {
		doc, _ := fetchDoc(t, aURL)
		w := doc.Ads[0].Wrapper
		if w == nil || len(w.Errors) == 0 {
			t.Fatal("wrapper with injected error URI expected")
		}
		errURI := strings.TrimSpace(w.Errors[0].URI)
		tid := queryParam(t, errURI, "tid")
		fireBeacon(t, h, vast.ExpandURIMacros(errURI, vast.URIMacroValues{ErrorCode: 303}))
		harness.WaitFor(t, 15*time.Second, "303 recorded", func() bool {
			return h.MediaEventsByTrace(t, tid, "video", "error", "303") >= 1
		})
	})
}
