//go:build e2e

// Regression tests for the security-hardening pass: the control-plane endpoints
// must stay authenticated, and the gateway must ship security headers. These
// lock in the fixes so the holes can't silently come back.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// TestSecurityControlPlaneRequiresAuth — /v1/config and /v1/services were
// UNAUTHENTICATED (GET /v1/config leaked database.url with the password + allowed
// PUT/DELETE config mutation). They must now 401 without a session and 200 with a
// permitted one. If this fails, the control plane is public again.
func TestSecurityControlPlaneRequiresAuth(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)

	for _, path := range []string{"/v1/config", "/v1/services"} {
		if code, _ := secGet(t, h, path, ""); code != http.StatusUnauthorized {
			t.Errorf("%s WITHOUT auth = %d, want 401 — the control plane must not be public", path, code)
		}
	}

	// A leak check: even the status aside, an unauthenticated body must not carry
	// the DB URL.
	if _, body := secGet(t, h, "/v1/config", ""); bytes.Contains([]byte(body), []byte("database.url")) {
		t.Error("/v1/config leaked database.url to an unauthenticated caller")
	}

	// With an admin bearer it works (so the staff Config UI still functions).
	tok := secMintAdminToken(t, h)
	if tok == "" {
		t.Fatal("could not mint an admin token (dev endpoint) to test the authenticated path")
	}
	if code, _ := secGet(t, h, "/v1/config", tok); code != http.StatusOK {
		t.Errorf("/v1/config WITH admin token = %d, want 200", code)
	}
}

// TestSecurityHeadersPresent — the gateway must emit baseline security headers on
// every response, and HSTS only when the request arrived over HTTPS (direct or
// X-Forwarded-Proto behind the ingress) so local plain-HTTP dev isn't pinned.
func TestSecurityHeadersPresent(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)

	// Plain HTTP (no XFP): the three always-on headers present, HSTS ABSENT.
	hdr := secHead(t, h, nil)
	if got := hdr.Get("X-Frame-Options"); got != "SAMEORIGIN" {
		t.Errorf("X-Frame-Options = %q, want SAMEORIGIN", got)
	}
	if got := hdr.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if hdr.Get("Referrer-Policy") == "" {
		t.Error("Referrer-Policy header missing")
	}
	if hdr.Get("Strict-Transport-Security") != "" {
		t.Error("HSTS must be ABSENT over plain HTTP (would pin local dev to HTTPS)")
	}

	// Simulated HTTPS via X-Forwarded-Proto (what the TLS-terminating ingress
	// sets) → HSTS present.
	hdrTLS := secHead(t, h, map[string]string{"X-Forwarded-Proto": "https"})
	if hdrTLS.Get("Strict-Transport-Security") == "" {
		t.Error("HSTS must be set when X-Forwarded-Proto=https (request came in over TLS)")
	}
}

// --- helpers ---

func secGet(t *testing.T, h *harness.Harness, path, bearer string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.URLs.Gateway+path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := h.HTTP.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	return resp.StatusCode, string(b)
}

func secHead(t *testing.T, h *harness.Harness, extra map[string]string) http.Header {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.URLs.Gateway+"/login", nil)
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := h.HTTP.Do(req)
	if err != nil {
		t.Fatalf("GET /login: %v", err)
	}
	resp.Body.Close()
	return resp.Header
}

func secMintAdminToken(t *testing.T, h *harness.Harness) string {
	t.Helper()
	resp, err := h.HTTP.Post(h.URLs.Gateway+"/v1/auth/token", "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var out struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.Token
}
