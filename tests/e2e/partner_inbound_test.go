//go:build e2e

// Partner inbound authentication (PLAN #112 deferred sub-item): the exchange's
// EXTERNAL HTTP OpenRTB surface (/v1/openrtb/auction) authenticates the caller
// against a per-partner sandbox key (X-API-Key). In strict mode a missing/invalid
// key is rejected 401; a valid key authenticates and binds the request to the
// trusted partner account. The internal gRPC twin our own SSP uses is NOT gated —
// so normal auctions (harness RunAuction) are unaffected.
package e2e

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestPartnerInboundAuctionAuth(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)

	// Provision a partner + a sandbox key (reuses the #112 self-serve flow).
	staff := h.CreateStaff(t, fmt.Sprintf("inbound-staff-%d", time.Now().UnixNano()))
	staffClient := h.OwnerClient(t, staff.ID)
	_, p := partnerReq(t, staffClient, http.MethodPost, h.URLs.Gateway+routes.APIPartners,
		fmt.Sprintf(`{"name":"E2E InboundPartner %d","kind":"ssp"}`, time.Now().UnixNano()))
	pid, _ := p["id"].(string)
	email := fmt.Sprintf("inbound-%d@integrations.test", time.Now().UnixNano())
	_, prov := partnerReq(t, staffClient, http.MethodPost, h.URLs.Gateway+routes.APIPartnerProvision,
		fmt.Sprintf(`{"id":%q,"email":%q}`, pid, email))
	temp, _ := prov["temp_password"].(string)
	partnerClient := h.LoginAs(t, email, temp)
	_, gen := partnerReq(t, partnerClient, http.MethodPost, h.URLs.Gateway+routes.APIPartnerSandboxKeys, "")
	key, _ := gen["value"].(string)
	if !strings.HasPrefix(key, "sk_") {
		t.Fatalf("no sandbox key generated: %v", gen)
	}

	const body = `{"id":"pauth-e2e","imp":[{"id":"1","banner":{"format":[{"w":300,"h":250}]}}],"site":{"domain":"example.com"}}`
	postAuction := func(apiKey string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, h.URLs.Exchange+routes.OpenRTBAuction, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if apiKey != "" {
			req.Header.Set("X-API-Key", apiKey)
		}
		resp, err := h.HTTP.Do(req)
		if err != nil {
			t.Fatalf("post auction: %v", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}

	// Turn on strict inbound partner auth for the exchange pod (revert after).
	h.SetConfigForPod(t, "exchange.inbound_partner_auth_strict", "true", harness.PodExchange)
	t.Cleanup(func() {
		h.SetConfigForPod(t, "exchange.inbound_partner_auth_strict", "false", harness.PodExchange)
	})

	// Wait until BOTH the strict config AND the sandbox key have propagated to the
	// exchange (config invalidate + secrets warm-cache refresh): a no-key POST must
	// 401 (strict active) and the valid key must be accepted (not 401).
	harness.WaitFor(t, 45*time.Second, "strict inbound partner auth active + key cached", func() bool {
		return postAuction("") == http.StatusUnauthorized && postAuction(key) != http.StatusUnauthorized
	})

	// Negative cases, now that strict is confirmed active.
	if code := postAuction(""); code != http.StatusUnauthorized {
		t.Errorf("no key: got %d, want 401", code)
	}
	if code := postAuction("sk_not_a_real_key"); code != http.StatusUnauthorized {
		t.Errorf("bad key: got %d, want 401", code)
	}
	if code := postAuction(key); code == http.StatusUnauthorized {
		t.Errorf("valid partner key: got 401, want authenticated (non-401)")
	}
}
