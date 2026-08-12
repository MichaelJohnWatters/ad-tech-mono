//go:build e2e

// Session revocation end-to-end: POST /v1/auth/revoke-sessions must kill every
// outstanding token for the caller (log-out-everywhere / stolen-token response),
// so a token that worked a moment ago is rejected on its next request. This is
// the live proof of pkg/middleware.RevocationStore wired into the gateway's Auth
// middleware — a regression that stopped honouring the checkpoint would silently
// make revocation a no-op, and only an end-to-end check catches that.
package e2e

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestSessionRevocation(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)

	uniq := fmt.Sprintf("revoke-%d", time.Now().UnixNano())
	adv := h.Signup(t, "Revoke Adv", uniq+"@api.test", "pw-e2e-1", "advertiser")

	// Pull the real signed JWT out of the jar; use it as a Bearer token so the
	// state-changing revoke POST isn't blocked by CSRF (Bearer/no-cookie passes).
	gw, _ := url.Parse(h.URLs.Gateway)
	var token string
	for _, c := range adv.Jar.Cookies(gw) {
		if c.Name == "adtech_session" {
			token = c.Value
		}
	}
	if token == "" {
		t.Fatal("no adtech_session cookie after signup")
	}

	client := harness.NewHTTPClient(10 * time.Second)
	call := func(t *testing.T, method, path string) int {
		t.Helper()
		req, _ := http.NewRequest(method, h.URLs.Gateway+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// Control: the fresh token authenticates.
	if got := call(t, http.MethodGet, routes.APINotifications); got != http.StatusOK {
		t.Fatalf("before revoke = %d, want 200 (fresh token must work)", got)
	}

	// The revocation cutoff is second-granular; ensure the token's iat second is
	// strictly before the cutoff so IsRevoked (issuedAt < cutoff) fires — a fresh
	// login in the SAME second is intentionally NOT revoked.
	time.Sleep(1100 * time.Millisecond)

	if got := call(t, http.MethodPost, routes.AuthRevokeSessions); got != http.StatusOK {
		t.Fatalf("revoke-sessions = %d, want 200", got)
	}

	// The same token — valid signature, unexpired — is now rejected.
	if got := call(t, http.MethodGet, routes.APINotifications); got != http.StatusUnauthorized {
		t.Errorf("after revoke = %d, want 401 (revoked session must be rejected)", got)
	}
}
