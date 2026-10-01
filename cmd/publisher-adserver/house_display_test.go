package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/houseads"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

const houseDisplayHTML = `<a href="https://adtech.test"><div class="house-banner">Advertise with AdTech Mono</div></a>`

func houseDisplayFn() houseAdLookup {
	return houseAdFrom(houseads.HouseAd{
		ID: "33333333-3333-4333-8333-333333333333", Format: houseads.FormatDisplay,
		Name: "House Banner", Markup: houseDisplayHTML, Enabled: true, Weight: 1,
	})
}

// TestServeDisplayHouseAd covers the display no-bid → platform house-ad fill.
func TestServeDisplayHouseAd(t *testing.T) {
	placement := postgres.PlacementRow{Placement: models.Placement{Width: 300, Height: 250}}

	// Fallback ON + a configured display house ad → serves its HTML as a
	// display fill (source "house"), which the JS SDK renders verbatim.
	t.Run("serves configured house ad", func(t *testing.T) {
		d := &serveDeps{log: nullLogger(), stubFn: alwaysStub, houseAdFn: houseDisplayFn()}
		rec := httptest.NewRecorder()
		if !d.serveDisplayHouseAd(httptest.NewRequest("GET", "/v1/pubad/serve", nil), rec, nullLogger(), placement, "trace-1") {
			t.Fatal("expected serveDisplayHouseAd to serve (return true)")
		}
		var out struct {
			Source        string `json:"source"`
			HTML          string `json:"html"`
			Width, Height int
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("response not JSON: %v (%s)", err, rec.Body.String())
		}
		if out.Source != "house" || out.HTML != houseDisplayHTML || out.Width != 300 || out.Height != 250 {
			t.Errorf("unexpected fill: %+v", out)
		}
		// The SDK treats a response WITHOUT html/no_fill/nobid as a fill; ours
		// carries html and no no_fill flag.
		if strings.Contains(rec.Body.String(), "no_fill") {
			t.Error("house fill must not carry a no_fill flag")
		}
	})

	// Fallback ON but NO display house ad configured → does not serve (caller
	// writes the honest no-fill).
	t.Run("no house ad configured → honest no-fill", func(t *testing.T) {
		d := &serveDeps{log: nullLogger(), stubFn: alwaysStub, houseAdFn: noHouseAds}
		rec := httptest.NewRecorder()
		if d.serveDisplayHouseAd(httptest.NewRequest("GET", "/v1/pubad/serve", nil), rec, nullLogger(), placement, "trace-2") {
			t.Error("expected no serve when no display house ad is configured")
		}
	})

	// Fallback OFF → never serves, even with a house ad configured.
	t.Run("fallback off → never serves", func(t *testing.T) {
		d := &serveDeps{log: nullLogger(), stubFn: func() bool { return false }, houseAdFn: houseDisplayFn()}
		rec := httptest.NewRecorder()
		if d.serveDisplayHouseAd(httptest.NewRequest("GET", "/v1/pubad/serve", nil), rec, nullLogger(), placement, "trace-3") {
			t.Error("expected no serve when the fallback master switch is off")
		}
	})
}
