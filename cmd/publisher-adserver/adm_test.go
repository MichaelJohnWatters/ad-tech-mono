package main

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/vast"
)

const buyerVAST = `<VAST version="4.0"><Ad id="ext-1"><InLine>` +
	`<AdSystem>extbidder</AdSystem><AdTitle>BrandX</AdTitle>` +
	`<Impression><![CDATA[https://brandx.example/imp?p=4.2500]]></Impression>` +
	`<Creatives><Creative><Linear><Duration>00:00:15</Duration>` +
	`<MediaFiles><MediaFile delivery="progressive" type="video/mp4" width="640" height="360"><![CDATA[https://cdn.brandx.example/v.mp4]]></MediaFile></MediaFiles>` +
	`</Linear></Creative></Creatives></InLine></Ad></VAST>`

func admWinner(adm string) *sspVideoWinner {
	return &sspVideoWinner{
		TraceID: "trace-adm", Channel: "video", CreativeID: "cr-ext", CampaignID: "li-ext",
		PlacementID: "pl-1", PublisherID: "pub-1", AdvertiserID: "adv-1",
		AdvertiserDomain: "brandx.example", BidModel: "cpm", Currency: "USD",
		ClearingPrice: 4.25, Width: 640, Height: 360, DurationSeconds: 15,
		MediaURL: "http://localhost:8080/creatives/fallback.mp4", AdM: adm,
	}
}

// TestAdFromWinner_AdMPath: a winner carrying parseable VAST-in-adm serves
// THAT ad — buyer beacons preserved, platform signed trackers injected.
func TestAdFromWinner_AdMPath(t *testing.T) {
	w := admWinner(buyerVAST)
	spec := buildVASTSpec(w, macroCtxForWinner(w, "http://tracker:8083", "https://gateway.adtech.local"))
	r := httptest.NewRequest("GET", "/v1/pubad/video/vast", nil)

	ad, ok := adFromWinner(r, w, spec, true, "https://gateway.adtech.local", nullLogger())
	if !ok || ad.InLine == nil {
		t.Fatalf("adm path must serve, got ok=%v ad=%+v", ok, ad)
	}
	if ad.InLine.AdSystem.Name != "extbidder" {
		t.Errorf("must serve the BUYER's ad (AdSystem extbidder), got %q", ad.InLine.AdSystem.Name)
	}
	// Buyer impression preserved + platform impression injected.
	var sawBuyer, sawPlatform bool
	for _, imp := range ad.InLine.Impressions {
		if strings.Contains(imp.URI, "brandx.example") {
			sawBuyer = true
		}
		if strings.Contains(imp.URI, "/v1/t/imp") && urlIsHMACValid(t, strings.TrimSpace(imp.URI)) {
			sawPlatform = true
		}
	}
	if !sawBuyer || !sawPlatform {
		t.Errorf("want buyer + signed platform impressions, got %+v", ad.InLine.Impressions)
	}
	// Platform error URI with the bracket macro injected.
	if len(ad.InLine.Errors) == 0 || !strings.Contains(ad.InLine.Errors[0].URI, "ec=[ERRORCODE]") {
		t.Errorf("platform <Error> with ec=[ERRORCODE] not injected: %+v", ad.InLine.Errors)
	}
	// Buyer's external CDN MediaFile must NOT be re-hosted.
	mf := ad.InLine.Creatives.Creatives[0].Linear.MediaFiles.MediaFiles[0]
	if !strings.Contains(mf.URI, "cdn.brandx.example") {
		t.Errorf("external CDN media must stay untouched: %q", mf.URI)
	}
}

// TestAdFromWinner_Fallbacks: garbage adm → local MediaURL build (warn);
// consume_adm off → local build even with valid adm; neither adm nor
// MediaURL → no ad.
func TestAdFromWinner_Fallbacks(t *testing.T) {
	r := httptest.NewRequest("GET", "/v1/pubad/video/vast", nil)

	garbage := admWinner("<VAST version=\"4.2\"><broken")
	spec := buildVASTSpec(garbage, macroCtxForWinner(garbage, "http://tracker:8083", "https://gateway.adtech.local"))
	ad, ok := adFromWinner(r, garbage, spec, true, "https://gateway.adtech.local", nullLogger())
	if !ok || ad.InLine == nil || ad.InLine.AdSystem.Name != "ad-tech-mono" {
		t.Fatalf("garbage adm must fall back to the local build, got %+v", ad)
	}

	valid := admWinner(buyerVAST)
	spec = buildVASTSpec(valid, macroCtxForWinner(valid, "http://tracker:8083", "https://gateway.adtech.local"))
	ad, ok = adFromWinner(r, valid, spec, false, "https://gateway.adtech.local", nullLogger())
	if !ok || ad.InLine.AdSystem.Name != "ad-tech-mono" {
		t.Fatalf("consume_adm=false must use the local build, got %+v", ad)
	}

	bare := admWinner("")
	bare.MediaURL = ""
	spec = buildVASTSpec(bare, macroCtxForWinner(bare, "http://tracker:8083", "https://gateway.adtech.local"))
	if _, ok := adFromWinner(r, bare, spec, true, "https://gateway.adtech.local", nullLogger()); ok {
		t.Fatal("no adm + no MediaURL must not produce an ad")
	}
}

// Keep the adserving import anchored (urlIsHMACValid lives in vast_test.go
// and uses it; this file references it indirectly via the helper).
var _ = adserving.DefaultSigningKey
var _ = vast.Version
