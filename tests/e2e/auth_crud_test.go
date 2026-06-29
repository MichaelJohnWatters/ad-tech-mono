//go:build e2e

// CRUD auth — verifies the AuthAPIKey middleware fires correctly on the
// DSP campaign and SSP placement management endpoints. Each test exercises
// one branch of the middleware decision tree against the live stack:
//
//   - missing X-API-Key header → 401
//   - unknown key value → 401
//   - revoked key → 401
//   - active key → 200 (the call succeeds, proving the wrapper passed
//     the request through to the underlying handler)
//   - rotating key (in grace window) → still 200
//
// The "unauthed reads also reject" assertion is important: pre-auth, the
// pub sim listed placements without any credential. Wrapping with auth
// closes that read leak — even non-mutating GETs require a key now.
package e2e

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// doManagementGet hits a CRUD endpoint with optional X-API-Key. Returns
// the status code; body discarded because the auth assertion only cares
// about the response code.
func doManagementGet(t *testing.T, h *harness.Harness, url, apiKey string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := h.HTTP.Do(req)
	if err != nil {
		t.Fatalf("call %s: %v", url, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// TestAuthCRUD_SSPPlacementsRejectsMissingKey — no X-API-Key header on a
// management endpoint must return 401. Pre-auth, this was a 200.
func TestAuthCRUD_SSPPlacementsRejectsMissingKey(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	status := doManagementGet(t, h, h.URLs.SSP+routes.SSPPlacements, "")
	if status != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401 (no X-API-Key)", status)
	}
}

// TestAuthCRUD_SSPPlacementsRejectsUnknownKey — header present but value
// not in the secrets cache returns 401. Proves the lookup is actually
// being performed, not just "header presence" check.
func TestAuthCRUD_SSPPlacementsRejectsUnknownKey(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	status := doManagementGet(t, h, h.URLs.SSP+routes.SSPPlacements,
		"this-key-does-not-exist-in-secrets")
	if status != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401 (unknown key)", status)
	}
}

// TestAuthCRUD_SSPPlacementsAcceptsDevKey — the seed-time dev key
// (DevAPIKey) must pass the middleware. Confirms the seed → secrets
// table → warm cache → middleware path all wired correctly.
func TestAuthCRUD_SSPPlacementsAcceptsDevKey(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	status := doManagementGet(t, h, h.URLs.SSP+routes.SSPPlacements,
		harness.DevAPIKey)
	if status != http.StatusOK {
		t.Errorf("status: got %d, want 200 (dev key)", status)
	}
}

// TestAuthCRUD_DSPCampaignsRejectsMissingKey — same as SSP but on the
// DSP management surface. Two services, same middleware, same posture.
func TestAuthCRUD_DSPCampaignsRejectsMissingKey(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	status := doManagementGet(t, h, h.URLs.DSP+routes.DSPCampaigns, "")
	if status != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", status)
	}
}

// TestAuthCRUD_DSPCampaignsAcceptsDevKey — dev key works on DSP too.
func TestAuthCRUD_DSPCampaignsAcceptsDevKey(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	status := doManagementGet(t, h, h.URLs.DSP+routes.DSPCampaigns,
		harness.DevAPIKey)
	if status != http.StatusOK {
		t.Errorf("status: got %d, want 200 (dev key)", status)
	}
}

// TestAuthCRUD_RevokedKeyRejected — insert a fresh active key, prove it
// works, revoke it, prove it stops working. Confirms the rotation/revoke
// path lands via NATS invalidate within the harness's cache-refresh
// window (RefreshCache call).
func TestAuthCRUD_RevokedKeyRejected(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)

	const name = "e2e-revoke-test-key"
	const value = "e2e-revoke-secret-value-xyz"
	t.Cleanup(func() { h.DeleteAPIKeysByName(t, name) })

	h.InsertAPIKey(t, name, value, "active")
	if got := doManagementGet(t, h, h.URLs.SSP+routes.SSPPlacements, value); got != http.StatusOK {
		t.Fatalf("active key: got %d, want 200", got)
	}

	h.RevokeAPIKey(t, name)
	if got := doManagementGet(t, h, h.URLs.SSP+routes.SSPPlacements, value); got != http.StatusUnauthorized {
		t.Errorf("after revoke: got %d, want 401", got)
	}
}

// TestAuthCRUD_RotatingKeyStillAccepted — insert a key with status=rotating
// directly, prove it still validates. Proves the grace-window semantics
// where the OLD value during rotation continues to authenticate until the
// revoke step.
func TestAuthCRUD_RotatingKeyStillAccepted(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)

	const name = "e2e-rotating-test-key"
	const value = "e2e-rotating-secret-value-abc"
	t.Cleanup(func() { h.DeleteAPIKeysByName(t, name) })

	h.InsertAPIKey(t, name, value, "rotating")
	if got := doManagementGet(t, h, h.URLs.SSP+routes.SSPPlacements, value); got != http.StatusOK {
		t.Errorf("rotating key: got %d, want 200 (grace window should accept old value)", got)
	}
}
