package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/houseads"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/native"
)

func testMacroCtx() adserving.MacroContext {
	return adserving.MacroContext{
		AuctionID:    "trace-1",
		AuctionPrice: 3.5,
		Currency:     "USD",
		CampaignID:   "li-native-001",
		CreativeID:   "cr-native-001",
		PlacementID:  "pl-1",
		PublisherID:  "pub-1",
		AdvertiserID: "adv-1",
		BidModel:     "cpm",
		TrackerURL:   "http://tracker:8083",
		LandingURL:   "http://landing.example/deal",
		URLTTL:       time.Hour,
	}
}

func TestRenderNativeHTML(t *testing.T) {
	resp := native.BuildResponse(native.AssetSet{
		Title:      "Acme Shoes 20% Off",
		MainImage:  "http://cdn.example/img.jpg",
		Icon:       "http://cdn.example/icon.png",
		Sponsored:  "Acme",
		Body:       "Free returns on every pair.",
		CTA:        "Shop now",
		LandingURL: "http://landing.example/deal",
	}, nil, nil)

	html, err := renderNativeHTML(resp, testMacroCtx())
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	// Every asset must appear in the rendered card.
	for _, want := range []string{
		"Acme Shoes 20% Off",          // title
		"http://cdn.example/img.jpg",  // main image
		"Acme",                        // sponsored
		"Free returns on every pair.", // body
		"Shop now",                    // CTA
	} {
		if !strings.Contains(html, want) {
			t.Errorf("rendered HTML missing %q\n%s", want, html)
		}
	}

	// The impression pixel and click link must be signed tracker URLs.
	if !strings.Contains(html, "/v1/t/imp?") || !strings.Contains(html, "sig=") {
		t.Errorf("expected a signed impression pixel, got:\n%s", html)
	}
	if !strings.Contains(html, "/v1/t/click?") {
		t.Errorf("expected a signed click URL, got:\n%s", html)
	}
	// The click tracker must redirect to the landing URL, not link to it directly.
	if strings.Contains(html, `href="http://landing.example/deal"`) {
		t.Errorf("click should route through the tracker, not link straight to landing:\n%s", html)
	}
}

func TestRenderNativeHTMLOmitsEmptyAssets(t *testing.T) {
	// A minimal native ad (title + landing only) must still render without
	// leaving empty tags for the absent image/sponsored/body/CTA.
	resp := native.BuildResponse(native.AssetSet{
		Title:      "Just a title",
		LandingURL: "http://l.example",
	}, nil, nil)

	html, err := renderNativeHTML(resp, testMacroCtx())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(html, "Just a title") {
		t.Errorf("missing title:\n%s", html)
	}
	if strings.Contains(html, "<img src=\"\"") {
		t.Errorf("rendered an empty image tag:\n%s", html)
	}
}

// nobidNativeSSP returns a no-bid for the native channel.
func nobidNativeSSP(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = encodeJSON(w, sspVideoWinner{NoBid: true})
	}))
}

// TestNativeHandlerNoBidHouseAd: on a no-bid with the fallback on AND a native
// house ad configured, the handler serves the house ad's own HTML markup
// verbatim (200 text/html) — not a hardcoded canned card.
func TestNativeHandlerNoBidHouseAd(t *testing.T) {
	ssp := nobidNativeSSP(t)
	defer ssp.Close()

	houseFn := houseAdFrom(houseads.HouseAd{
		ID: "33333333-3333-4333-8333-333333333333", Format: houseads.FormatNative,
		Name: "House Native", Markup: houseNativeHTML, Enabled: true, Weight: 1,
	})
	h := nativeHandler(nullLogger(), "http://tracker:8083", ssp.URL, "https://gateway.adtech.local", alwaysStub, houseFn)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/v1/pubad/native?placement_id=pl-1", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (house ad served): %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(rec.Body.String(), "Try AdTech Mono") {
		t.Errorf("expected the configured native house ad markup, got:\n%s", rec.Body.String())
	}
}

// TestNativeHandlerNoBidNoHouseAd: fallback on but NO native house ad configured
// → honest 204 (no fake/canned content invented).
func TestNativeHandlerNoBidNoHouseAd(t *testing.T) {
	ssp := nobidNativeSSP(t)
	defer ssp.Close()

	h := nativeHandler(nullLogger(), "http://tracker:8083", ssp.URL, "https://gateway.adtech.local", alwaysStub, noHouseAds)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/v1/pubad/native?placement_id=pl-1", nil))
	if rec.Code != http.StatusNoContent {
		t.Errorf("no native house ad → status = %d, want 204: %s", rec.Code, rec.Body.String())
	}
}
