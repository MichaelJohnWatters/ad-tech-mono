package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/publisheradserver/prebidclient"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

// TestWritePrebidWinner_SubstitutesAuctionMacros: the Prebid fan-out bypasses
// the exchange, so the §4.4 win-time substitution must happen in
// writePrebidWinner — an external bidder's adm carrying ${AUCTION_PRICE} /
// ${AUCTION_ID} serves with the clearing price and trace substituted, never
// the literal macro. The platform beacons still ride the wrapper.
func TestWritePrebidWinner_SubstitutesAuctionMacros(t *testing.T) {
	d := &serveDeps{
		log:        nullLogger(),
		clk:        clock.Real{},
		trackerURL: "http://tracker:8083",
		secureBase: "https://gateway.adtech.local",
	}
	best := prebidclient.Result{
		Endpoint: "http://prebid.example", Seat: "ext-seat", Price: 3.75, Currency: "USD",
		HTML: `<div data-buyer="1"><img src="https://buyer.example/imp?p=${AUCTION_PRICE}&a=${AUCTION_ID}&c=${AUCTION_CURRENCY}"/></div>`,
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/pubad/serve?placement_id=pl-1", nil)
	d.writePrebidWinner(req.Context(), req, rec, postgres.PlacementRow{}, "trace-prebid-1", best)

	var out struct {
		HTML string `json:"html"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body.String())
	}
	if strings.Contains(out.HTML, "${AUCTION") {
		t.Fatalf("literal auction macro served to the page:\n%s", out.HTML)
	}
	for _, want := range []string{"p=3.7500", "a=trace-prebid-1", "c=USD", "data-buyer=\"1\"", "data-prebid-wrapper"} {
		if !strings.Contains(out.HTML, want) {
			t.Errorf("wrapped HTML missing %q:\n%s", want, out.HTML)
		}
	}
	if !strings.Contains(out.HTML, "/v1/t/imp") {
		t.Errorf("platform impression beacon missing from wrapper:\n%s", out.HTML)
	}
}
