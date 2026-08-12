//go:build e2e

package harness

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// SetConfigForPod writes a live config value for a specific pod via
// PUT /v1/config. The gateway's pkg/config.Manager persists the value
// to Postgres (keyed by pod_id, key) and broadcasts adtech.cache.invalidate.config
// on NATS. The target pod's manager subscribes to that subject and
// re-polls on receipt, so the change lands within NATS round-trip time
// (~ms in local Tilt).
//
// Why pod_id matters: every pod seeds its live-tier defaults into a
// pod-scoped row at boot (see registry.Register → UpdateForPod with
// r.podID). FetchAllForPod merges pod-scoped on top of global rows, so a
// global-scope PUT is shadowed by the pod's own seeded row. To make a
// change visible to a specific service you must write to its pod_id.
//
// Conventional podID values for Tilt-managed services: "exchange-0",
// "dsp-internal-0", "ssp-0", etc. (see Tiltfile POD_NAME env vars).
//
// Cleanup is the caller's responsibility — typically via t.Cleanup() that
// SetConfig's the key back to its original value. Reset() does NOT
// truncate the config table (config outlives test data).
func (h *Harness) SetConfigForPod(t *testing.T, key, value, podID string) {
	t.Helper()

	resp, err := h.putConfig(key, value, podID)
	if err != nil {
		t.Fatalf("PUT %s: %v", routes.Config, err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /v1/config status %d: %s", resp.StatusCode, string(respBody))
	}

	// NATS round-trip + subscriber re-poll. Local NATS is usually <5ms but
	// the subscriber also has to do a Postgres FetchAll.
	time.Sleep(300 * time.Millisecond)
}

// putConfig issues the config PUT with the same port-forward-flap retry as
// refreshOne — a transient EOF on the gateway tunnel must not fail a test
// (or worse, poison a cleanup restore into writing the schema default over
// a seeded per-pod value). The PUT is idempotent, so re-sending is safe.
func (h *Harness) putConfig(key, value, podID string) (*http.Response, error) {
	body, _ := json.Marshal(map[string]any{
		"key":    key,
		"value":  value,
		"pod_id": podID,
	})
	var resp *http.Response
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Second)
		}
		var req *http.Request
		// h.HTTP's own 10s timeout bounds each attempt (a request context
		// would have to outlive the body read, so we don't use one here).
		req, err = http.NewRequest(http.MethodPut,
			h.URLs.Gateway+routes.Config, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		// /v1/config is auth-gated (config:update for PUT). Authenticate with an
		// admin bearer minted from the dev token endpoint (available because the
		// e2e stack runs debug.endpoints_enabled=true).
		if tok := h.adminBearer(); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err = h.HTTP.Do(req)
		if err == nil {
			return resp, nil
		}
	}
	return nil, err
}

// adminBearer mints (once, cached) an admin JWT via the dev token endpoint and
// caches it on the harness. Used to authenticate the auth-gated /v1/config PUTs.
// Best-effort: returns "" on failure (the caller's PUT then fails loudly, which
// is the right signal — the endpoint IS gated).
func (h *Harness) adminBearer() string {
	if h.adminTok != "" {
		return h.adminTok
	}
	resp, err := h.HTTP.Post(h.URLs.Gateway+routes.AuthToken, "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var out struct {
		Token string `json:"token"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) == nil {
		h.adminTok = out.Token
	}
	return h.adminTok
}

// AdminToken mints (and caches) an admin JWT via the dev token endpoint — a
// platform (staff) session for tests that need a privileged caller (e.g. a
// staff-only endpoint). Fails the test if the endpoint is unavailable (the e2e
// stack runs debug.endpoints_enabled=true, so it should always be reachable).
func (h *Harness) AdminToken(t *testing.T) string {
	t.Helper()
	tok := h.adminBearer()
	if tok == "" {
		t.Fatal("could not mint admin token — is the dev token endpoint enabled?")
	}
	return tok
}

// RestoreConfigForPod is the cleanup counterpart of SetConfigForPod. Tests
// capture a key's resolved value BEFORE mutating it and write it back in
// t.Cleanup — but the captured value can be empty (row absent / pod not yet
// re-seeded) or even a leftover invalid value from an aborted run, and the
// gateway now schema-validates PUTs, so restoring it verbatim 400s and fails
// the test at teardown. Try the original when non-empty; on rejection (or
// empty) fall back to the schema default the caller provides.
func (h *Harness) RestoreConfigForPod(t *testing.T, key, original, schemaDefault, podID string) {
	t.Helper()
	if original != "" && h.tryPutConfig(t, key, original, podID) {
		return
	}
	h.SetConfigForPod(t, key, schemaDefault, podID)
}

// tryPutConfig issues one config PUT and reports success without failing the
// test — the restore path uses it to fall back on a rejected original.
func (h *Harness) tryPutConfig(t *testing.T, key, value, podID string) bool {
	t.Helper()
	resp, err := h.putConfig(key, value, podID)
	if err != nil {
		t.Logf("restore of %s=%q failed after retries (%v) — falling back to schema default", key, value, err)
		return false
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		time.Sleep(300 * time.Millisecond) // same propagation beat as SetConfigForPod
		return true
	}
	t.Logf("restore of %s=%q rejected (status %d) — falling back to schema default", key, value, resp.StatusCode)
	return false
}

// DeleteConfig removes a live config key via DELETE /v1/config (all pod
// rows for the key). The gateway's manager broadcasts the config
// invalidate, and every pod's next snapshot poll DROPS the key — reads
// revert to env/schema default. This is the behaviour under test in
// TestConfigDeleteRevertsLiveValue: deleting a row used to pin the
// last-known value in every pod until restart.
func (h *Harness) DeleteConfig(t *testing.T, key string) {
	t.Helper()
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Second)
		}
		req, err := http.NewRequest(http.MethodDelete,
			h.URLs.Gateway+routes.Config+"?key="+key, nil)
		if err != nil {
			t.Fatalf("DELETE %s: %v", routes.Config, err)
		}
		if tok := h.adminBearer(); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := h.HTTP.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("DELETE /v1/config status %d: %s", resp.StatusCode, string(body))
		}
		// NATS invalidate + subscriber re-poll, same margin as SetConfigForPod.
		time.Sleep(300 * time.Millisecond)
		return
	}
	t.Fatalf("DELETE /v1/config: %v", lastErr)
}

// ResolvedConfig mirrors the gateway's /v1/config?resolved=true response.
type ResolvedConfig struct {
	Value  string `json:"value"`
	Source string `json:"source"`
	PodID  string `json:"pod_id"`
}

// GetResolvedConfig reads a key's resolved value + provenance for a pod via
// the gateway, AUTHENTICATED — /v1/config is auth-gated since the security
// hardening, and the old unauthenticated read decoded the 401 error body
// into an empty struct, which made three seed-defaults tests fail with
// value="" while the config system was perfectly healthy. Fails the test on
// any non-200 so an auth regression is loud instead of silently empty.
func (h *Harness) GetResolvedConfig(t *testing.T, key, pod string) ResolvedConfig {
	t.Helper()
	url := h.URLs.Gateway + routes.Config + "?resolved=true&key=" + key + "&pod=" + pod
	// Retry transport flaps (port-forward EOF/reset under load) like refreshOne
	// — GetResolvedConfig runs in test setup (e.g. MakeDSPAlwaysNoBid), so a
	// single-shot failure fast-fails the whole test. Rebuild the authed request
	// each attempt; GET is idempotent.
	var resp *http.Response
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Second)
		}
		var req *http.Request
		req, err = http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			t.Fatalf("build resolved-config request: %v", err)
		}
		if tok := h.adminBearer(); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err = h.HTTP.Do(req)
		if err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("resolved config get %s after retries: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resolved config %s status %d: %s", url, resp.StatusCode, string(body))
	}
	var out ResolvedConfig
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode resolved config: %v\nbody: %s", err, string(body))
	}
	return out
}
