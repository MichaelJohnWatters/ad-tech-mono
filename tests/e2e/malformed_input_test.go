//go:build e2e

// Malformed / invalid requests must be rejected cleanly at the edge with a 4xx,
// never a 500 or a silent accept. Unit tests cover the parsers; this proves the
// live HTTP boundary (external OpenRTB ingress + the portal campaign API) turns
// bad input into a deterministic client error rather than leaking a stack trace
// or a raw Postgres cast error.
package e2e

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestMalformedInputRejected(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)

	// --- External OpenRTB (Prebid) ingress ---

	t.Run("prebid_invalid_json_400", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, h.URLs.Exchange+routes.PrebidAuction,
			strings.NewReader(`{"id":"x","imp":[ this is not json `))
		req.Header.Set("Content-Type", "application/json")
		resp, err := h.HTTP.Do(req)
		if err != nil {
			t.Fatalf("prebid post: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("prebid invalid JSON = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("prebid_wrong_method_405", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, h.URLs.Exchange+routes.PrebidAuction, nil)
		resp, err := h.HTTP.Do(req)
		if err != nil {
			t.Fatalf("prebid get: %v", err)
		}
		resp.Body.Close()
		// 405 when the endpoint is enabled; 503 when prebid is disabled by config.
		// Either way it must not 200 or 500.
		if resp.StatusCode != http.StatusMethodNotAllowed && resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("prebid GET = %d, want 405 (or 503 if disabled)", resp.StatusCode)
		}
	})

	// --- Portal campaign API ---

	h.Reset(t)
	uniq := fmt.Sprintf("malformed-%d", time.Now().UnixNano())
	adv := h.Signup(t, "Malformed Adv", uniq+"@api.test", "pw-e2e-1", "advertiser")

	t.Run("campaign_missing_required_name_400", func(t *testing.T) {
		got := h.APIStatus(t, adv, http.MethodPost, "/v1/api/campaigns",
			`{"base_bid":1.5,"daily_budget":100}`)
		if got != http.StatusBadRequest {
			t.Errorf("create without name = %d, want 400", got)
		}
	})

	t.Run("campaign_bad_uuid_path_is_4xx_not_5xx", func(t *testing.T) {
		// A non-UUID id must not surface a raw `invalid input syntax for uuid`
		// 500 — the handler should treat it as not-found / bad-request.
		got := h.APIStatus(t, adv, http.MethodPatch, "/v1/api/campaigns/not-a-uuid",
			`{"base_bid":2.0}`)
		if got < 400 || got >= 500 {
			t.Errorf("PATCH with non-UUID id = %d, want a 4xx (not 5xx / not a leaked DB error)", got)
		}
	})

	t.Run("campaign_invalid_json_body_400", func(t *testing.T) {
		got := h.APIStatus(t, adv, http.MethodPost, "/v1/api/campaigns", `{"name": "x", `)
		if got != http.StatusBadRequest {
			t.Errorf("create with truncated JSON = %d, want 400", got)
		}
	})
}
