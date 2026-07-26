//go:build e2e

// A tampered or forged session cookie must be rejected with 401 — the signature
// check in pkg/middleware.Auth is the whole point of signing the JWT. Nothing
// asserted this end to end; a regression that accepted an unverified cookie
// would let anyone forge a session for any account.
package e2e

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestSessionCookieTamperRejected(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)

	uniq := fmt.Sprintf("sess-%d", time.Now().UnixNano())
	adv := h.Signup(t, "Sess Adv", uniq+"@api.test", "pw-e2e-1", "advertiser")

	// Pull the real signed session cookie out of the jar.
	gw, _ := url.Parse(h.URLs.Gateway)
	var real string
	for _, c := range adv.Jar.Cookies(gw) {
		if c.Name == "adtech_session" {
			real = c.Value
		}
	}
	if real == "" {
		t.Fatal("no adtech_session cookie after signup")
	}

	// get issues a request carrying exactly the given session cookie value on a
	// jar-less client (so nothing else leaks in) and returns the status.
	get := func(t *testing.T, cookieVal string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, h.URLs.Gateway+"/v1/api/notifications", nil)
		req.AddCookie(&http.Cookie{Name: "adtech_session", Value: cookieVal})
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	t.Run("valid_cookie_ok", func(t *testing.T) {
		if got := get(t, real); got != http.StatusOK {
			t.Errorf("valid session = %d, want 200 (control — proves the endpoint is reachable)", got)
		}
	})

	t.Run("tampered_signature_rejected", func(t *testing.T) {
		if got := get(t, flipLastByte(real)); got != http.StatusUnauthorized {
			t.Errorf("tampered session = %d, want 401 (signature must not verify)", got)
		}
	})

	t.Run("garbage_cookie_rejected", func(t *testing.T) {
		if got := get(t, "not.a.valid.jwt"); got != http.StatusUnauthorized {
			t.Errorf("garbage session = %d, want 401", got)
		}
	})
}

// flipLastByte returns s with its final character changed, breaking a JWT's
// trailing signature bytes so validation fails.
func flipLastByte(s string) string {
	if s == "" {
		return "x"
	}
	b := []byte(s)
	if b[len(b)-1] == 'A' {
		b[len(b)-1] = 'B'
	} else {
		b[len(b)-1] = 'A'
	}
	return string(b)
}
