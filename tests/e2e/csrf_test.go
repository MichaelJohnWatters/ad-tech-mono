//go:build e2e

// CSRF defense-in-depth (pkg/middleware/csrf.go) wraps the whole gateway mux
// but had no e2e. It must reject a cross-site, cookie-authenticated,
// state-changing request — while leaving legitimate same-origin and non-browser
// (no-Origin / token) traffic untouched. A regression that silently disabled it
// would reopen the residual CSRF vectors SameSite=Lax doesn't cover.
package e2e

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestCSRFCrossSiteBlocked(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)

	// Signup returns a cookie-jar client (the browser case CSRF guards). A raw
	// request through it carries the adtech_session cookie but no Authorization
	// header — exactly the shape CSRF inspects.
	uniq := fmt.Sprintf("csrf-%d", time.Now().UnixNano())
	adv := h.Signup(t, "CSRF Adv", uniq+"@api.test", "pw-e2e-1", "advertiser")

	post := func(t *testing.T, origin string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, h.URLs.Gateway+"/v1/api/campaigns",
			strings.NewReader(`{"name":"csrf probe","base_bid":1,"daily_budget":10}`))
		req.Header.Set("Content-Type", "application/json")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := adv.Do(req)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	t.Run("cross_site_origin_blocked", func(t *testing.T) {
		if got := post(t, "https://evil.example.com"); got != http.StatusForbidden {
			t.Errorf("cross-site cookie POST = %d, want 403 (CSRF must block it)", got)
		}
	})

	// The two legitimate shapes must NOT be blocked by CSRF (they may 4xx/2xx on
	// their own merits, but never the 403 cross-site block).
	t.Run("absent_origin_not_csrf_blocked", func(t *testing.T) {
		if got := post(t, ""); got == http.StatusForbidden {
			t.Errorf("no-Origin cookie POST = 403; a non-browser cookie client must pass CSRF (SameSite covers browsers)")
		}
	})
	t.Run("same_origin_not_csrf_blocked", func(t *testing.T) {
		if got := post(t, h.URLs.Gateway); got == http.StatusForbidden {
			t.Errorf("same-origin cookie POST = 403; must be allowed")
		}
	})

	// Safe methods are never CSRF-checked even cross-site.
	t.Run("cross_site_get_allowed", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, h.URLs.Gateway+"/v1/api/campaigns", nil)
		req.Header.Set("Origin", "https://evil.example.com")
		resp, err := adv.Do(req)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusForbidden {
			t.Errorf("cross-site GET = 403; safe methods must be exempt from CSRF")
		}
	})
}
