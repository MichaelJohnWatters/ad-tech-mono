//go:build e2e

// OIDC SSO (PLAN Phase 11 #110), live surface: per-account SSO config CRUD is
// owner-gated (sso:manage), the client_secret is never returned, and the public
// /v1/auth/sso/{start,callback} endpoints reject bad input (invalid account, no
// flow). The full IdP round-trip (real go-oidc id_token verification + JIT) is
// covered by the in-process pkg/ssoauth test — the in-cluster gateway can't reach a
// host-run fake IdP.
package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestSSOConfigAndPublicEndpoints(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "sso")
	adv := w.AdvAcc
	owner := h.OwnerClient(t, adv.ID)
	cfgURL := h.URLs.Gateway + routes.APIAccountSSO

	putCfg := func(cl *http.Client, body string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPut, cfgURL, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatalf("put sso config: %v", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	getCfg := func(cl *http.Client) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, cfgURL, nil)
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatalf("get sso config: %v", err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return resp.StatusCode, m
	}

	// Owner configures OIDC SSO.
	valid := `{"enabled":true,"issuer":"https://idp.example.com","client_id":"cid","client_secret":"shh","allowed_domains":["corp.com"],"default_role":"viewer"}`
	if st := putCfg(owner, valid); st != 200 {
		t.Fatalf("owner PUT valid config: %d", st)
	}
	// GET reflects it — and NEVER returns the client_secret.
	st, cfg := getCfg(owner)
	if st != 200 {
		t.Fatalf("owner GET config: %d", st)
	}
	if cfg["enabled"] != true || cfg["issuer"] != "https://idp.example.com" || cfg["has_client_secret"] != true {
		t.Errorf("config not reflected: %v", cfg)
	}
	if _, leaked := cfg["client_secret"]; leaked {
		t.Errorf("client_secret LEAKED in the config API: %v", cfg["client_secret"])
	}

	// Validation: enabling without a domain allowlist (which would deny all) → 400;
	// a privileged default_role → 400.
	if st := putCfg(owner, `{"enabled":true,"issuer":"https://idp.example.com","client_id":"cid","allowed_domains":[],"default_role":"viewer"}`); st != http.StatusBadRequest {
		t.Errorf("enable without domains: got %d, want 400", st)
	}
	if st := putCfg(owner, `{"enabled":true,"issuer":"https://idp.example.com","client_id":"cid","allowed_domains":["corp.com"],"default_role":"owner"}`); st != http.StatusBadRequest {
		t.Errorf("privileged default_role: got %d, want 400", st)
	}

	// RBAC: a non-owner team member (viewer, no sso:manage) cannot configure SSO.
	viewerEmail := fmt.Sprintf("sso-viewer-%d@e2e.local", time.Now().UnixNano())
	h.CreateLoginUser(t, adv.ID, viewerEmail, "e2e-pass", "viewer")
	viewer := h.LoginAs(t, viewerEmail, "e2e-pass")
	if st := putCfg(viewer, valid); st != http.StatusForbidden {
		t.Errorf("viewer PUT config: got %d, want 403", st)
	}
	if st, _ := getCfg(viewer); st != http.StatusForbidden {
		t.Errorf("viewer GET config: got %d, want 403", st)
	}

	// Public endpoints reject bad input. Use a non-redirect-following client so we
	// can observe the 3xx.
	noRedir := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	getRaw := func(path string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, h.URLs.Gateway+path, nil)
		resp, err := noRedir.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		return resp
	}

	// /sso/start with a non-UUID account → 400.
	if resp := getRaw(routes.AuthSSOStart + "?account=not-a-uuid"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("sso start bad account: got %d, want 400", resp.StatusCode)
		resp.Body.Close()
	} else {
		resp.Body.Close()
	}
	// /sso/callback with no flow cookie → 400 (CSRF/no-flow).
	if resp := getRaw(routes.AuthSSOCallback + "?code=x&state=y"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("sso callback no-flow: got %d, want 400", resp.StatusCode)
		resp.Body.Close()
	} else {
		resp.Body.Close()
	}

	// A fresh account with SSO NOT enabled → /sso/start redirects to /login.
	w2 := harness.BuildBasicWorld(t, h, "sso2")
	resp := getRaw(routes.AuthSSOStart + "?account=" + w2.AdvAcc.ID)
	loc := resp.Header.Get("Location")
	resp.Body.Close()
	if resp.StatusCode/100 != 3 || loc != "/login" {
		t.Errorf("sso start (not enabled): got %d loc=%q, want 3xx → /login", resp.StatusCode, loc)
	}
}
