//go:build e2e

// The gateway must strip inbound gateway-trusted identity headers at the edge so
// a client can't smuggle one past an UNAUTHENTICATED pass-through proxy. The
// /v1/reporting/ proxy is CORS-only (no auth); reporting's trace endpoint derives
// tenant scope + redaction purely from X-Account-* headers the gateway is meant
// to inject after authenticating. A raw request forging `X-Account-Type: staff`
// must NOT be treated as a platform operator — before the fix it returned
// unscoped, un-redacted cross-tenant data with no auth at all.
package e2e

import (
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestGatewayStripsForgedIdentityHeaders(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)

	req, _ := http.NewRequest(http.MethodGet, h.URLs.Gateway+"/v1/reporting/trace?trace_id=forged-probe", nil)
	// A client forging a staff identity + a victim account.
	req.Header.Set("X-Account-Type", "staff")
	req.Header.Set("X-Account-ID", "00000000-0000-0000-0000-000000000000")
	req.Header.Set("X-User-ID", "attacker")

	resp, err := harness.NewHTTPClient(10 * time.Second).Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	// The strip leaves reporting with no account scope, so its scope resolver
	// denies (403). The security invariant is simply: the forged staff header is
	// NOT honoured — never a 200 that would have carried cross-tenant data.
	if resp.StatusCode == http.StatusOK {
		t.Errorf("forged X-Account-Type: staff was honoured (200) — identity header not stripped at the edge")
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Logf("note: got %d (403 expected from the scope resolver); test passes as long as it isn't 200-with-data", resp.StatusCode)
	}
}
