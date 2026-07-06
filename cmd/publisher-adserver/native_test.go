package main

import (
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
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

func TestStubNativeRenders(t *testing.T) {
	// The demo fallback must always produce a valid, non-empty card.
	resp, ctx := stubNative("http://tracker:8083", "trace-x", "pl-demo")
	html, err := renderNativeHTML(resp, ctx)
	if err != nil {
		t.Fatalf("render stub: %v", err)
	}
	if !strings.Contains(html, "Acme Running Shoes") {
		t.Errorf("stub native missing expected title:\n%s", html)
	}
}
