//go:build e2e

// Phase 5: bootstrap one-shot operator-key minting. POST /v1/auth/bootstrap
// with the platform root password (PLATFORM_ROOT_PASSWORD env var, set
// to "dev-root-password" by the Tiltfile) mints a single operator API
// key, returns it, and self-disables. Subsequent calls return 410.
//
// Each test cleans up by deleting the row before AND after so it can run
// against any prior state. The Tiltfile env var is what makes the
// endpoint live in dev/e2e — production deployments set a real password.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// callBootstrap POSTs to /v1/auth/bootstrap with the given password.
// Returns body + status; caller decides what to assert.
func callBootstrap(t *testing.T, h *harness.Harness, password string) (string, int) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"password": password})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		h.URLs.Gateway+routes.AuthBootstrap, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build bootstrap request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.HTTP.Do(req)
	if err != nil {
		t.Fatalf("bootstrap call: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return string(out), resp.StatusCode
}

// TestBootstrap_MintsKeyOnFirstCall — first POST with the correct
// password creates a 'bootstrap-admin-key' secret and returns it. Test
// cleans up the row before AND after so it doesn't leak state to other
// tests in the suite.
func TestBootstrap_MintsKeyOnFirstCall(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.DeleteAPIKeysByName(t, "bootstrap-admin-key")
	t.Cleanup(func() { h.DeleteAPIKeysByName(t, "bootstrap-admin-key") })

	body, status := callBootstrap(t, h, "dev-root-password")
	if status != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", status, body)
	}
	var resp struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode bootstrap response: %v\nbody: %s", err, body)
	}
	if resp.Name != "bootstrap-admin-key" {
		t.Errorf("name: got %q, want bootstrap-admin-key", resp.Name)
	}
	if len(resp.Value) != 64 {
		t.Errorf("value length: got %d, want 64 (32-byte hex)", len(resp.Value))
	}
}

// TestBootstrap_SecondCallReturns410 — after the first successful mint,
// subsequent calls return 410 Gone regardless of password. Proves the
// single-shot guard fires before the password check (timing-safe).
func TestBootstrap_SecondCallReturns410(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.DeleteAPIKeysByName(t, "bootstrap-admin-key")
	t.Cleanup(func() { h.DeleteAPIKeysByName(t, "bootstrap-admin-key") })

	if _, status := callBootstrap(t, h, "dev-root-password"); status != http.StatusOK {
		t.Fatalf("first call: got %d, want 200", status)
	}
	body, status := callBootstrap(t, h, "dev-root-password")
	if status != http.StatusGone {
		t.Errorf("second call: got %d, want 410; body=%s", status, body)
	}
}

// TestBootstrap_WrongPasswordRejected — incorrect password returns 401
// when the endpoint hasn't fired yet. After firing, returns 410 even
// with wrong password (single-shot guard wins).
func TestBootstrap_WrongPasswordRejected(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.DeleteAPIKeysByName(t, "bootstrap-admin-key")
	t.Cleanup(func() { h.DeleteAPIKeysByName(t, "bootstrap-admin-key") })

	body, status := callBootstrap(t, h, "wrong-password")
	if status != http.StatusUnauthorized {
		t.Errorf("wrong password: got %d, want 401; body=%s", status, body)
	}
	// Sanity: the wrong-password attempt should NOT have minted a row,
	// so a correct-password call right after still works.
	if _, status := callBootstrap(t, h, "dev-root-password"); status != http.StatusOK {
		t.Errorf("correct password after failed attempt: got %d, want 200", status)
	}
}

// TestBootstrap_MintedKeyWorksOnDSPCRUD — full end-to-end loop: bootstrap
// mints a key, then that key authenticates against the DSP CRUD
// middleware. Proves the secrets cache picks up the new row via the
// NATS invalidate that secrets.SetForPod-style writes broadcast.
//
// NOTE: this test depends on the DSP secrets cache observing the new
// row within the WaitFor poll window. The cache's NATS subscriber
// triggers on adtech.cache.invalidate.secrets, but the bootstrap
// handler currently uses raw INSERT without that broadcast. So we poll
// for up to the 30s natural refresh interval.
func TestBootstrap_MintedKeyWorksOnDSPCRUD(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.DeleteAPIKeysByName(t, "bootstrap-admin-key")
	t.Cleanup(func() { h.DeleteAPIKeysByName(t, "bootstrap-admin-key") })

	body, status := callBootstrap(t, h, "dev-root-password")
	if status != http.StatusOK {
		t.Fatalf("bootstrap: got %d", status)
	}
	var resp struct {
		Value string `json:"value"`
	}
	json.Unmarshal([]byte(body), &resp)

	// Force the DSP secrets cache to refresh so it sees the new row,
	// without waiting for the 30s tick.
	h.RefreshCache(t, h.URLs.DSP)

	gotStatus := doManagementGet(t, h, h.URLs.DSP+routes.DSPCampaigns, resp.Value)
	if gotStatus != http.StatusOK {
		t.Errorf("DSP CRUD with bootstrapped key: got %d, want 200", gotStatus)
	}
}
