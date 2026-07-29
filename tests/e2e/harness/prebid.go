//go:build e2e

package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// PostPrebidAuction POSTs a Prebid-compatible OpenRTB 2.x bid request to
// the exchange's /v1/prebid/openrtb2/auction endpoint. Returns the decoded
// BidResponse and the raw HTTP status — tests assert on whichever they
// care about.
//
// The wire format is identical to internal openrtb.BidRequest (Prebid
// speaks OpenRTB), so callers reuse the same types they'd use against
// /v1/openrtb/auction directly.
func (h *Harness) PostPrebidAuction(t *testing.T, req openrtb.BidRequest) (openrtb.BidResponse, int) {
	t.Helper()
	// A real Prebid Server sends a SupplyChain (schain) declaring the upstream
	// path — under the prod-shaped strict schain enforcement the exchange no-bids
	// requests without one. Inject a well-formed node if the test didn't set one,
	// exactly as an authorised Prebid publisher would. (The request's site.domain
	// is a harness publisher, already ads.txt-authorised via RefreshAllCaches.)
	if openrtb.SChainOf(&req) == nil {
		if req.Source == nil {
			req.Source = &openrtb.Source{}
		}
		if req.Source.Ext == nil {
			req.Source.Ext = &openrtb.SourceExt{}
		}
		req.Source.Ext.SChain = &openrtb.SupplyChain{
			Ver: openrtb.SChainVersion, Complete: 1,
			Nodes: []openrtb.SupplyChainNode{{ASI: "prebid.example", SID: "pbs-1", HP: 1}},
		}
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal prebid request: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := h.URLs.Exchange + routes.PrebidAuction
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build prebid request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := h.HTTP.Do(httpReq)
	if err != nil {
		t.Fatalf("call prebid: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		// Caller might be asserting on a non-200 (e.g. disabled endpoint
		// test). Return the status so they can branch — only fail if there's
		// no obvious response shape to decode.
		return openrtb.BidResponse{}, resp.StatusCode
	}

	var bidResp openrtb.BidResponse
	if err := json.Unmarshal(respBody, &bidResp); err != nil {
		t.Fatalf("decode prebid response: %v\nbody: %s", err, string(respBody))
	}
	return bidResp, resp.StatusCode
}
