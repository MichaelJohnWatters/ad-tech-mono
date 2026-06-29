//go:build e2e

// CRUD coverage for the gateway /v1/api/secrets endpoint that backs
// the Secrets sub-tab in the console UI. Each test cleans up after
// itself so the suite doesn't drift state across runs.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// jsonReq wraps the boilerplate of building an authenticated JSON
// request against the secrets endpoint. apiKey="" omits the header
// (so we can also test missing-auth).
func jsonReq(t *testing.T, h *harness.Harness, method, path string, body any, apiKey string) (*http.Response, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		buf, _ := json.Marshal(body)
		rdr = bytes.NewReader(buf)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, h.URLs.Gateway+path, rdr)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := h.HTTP.Do(req)
	if err != nil {
		t.Fatalf("call %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

// TestSecretsCRUD_RejectsMissingKey — sanity: every secrets endpoint
// is auth-gated. Without X-API-Key all methods must return 401.
func TestSecretsCRUD_RejectsMissingKey(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete} {
		resp, _ := jsonReq(t, h, method, routes.APISecrets, nil, "")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without key: got %d, want 401", method, resp.StatusCode)
		}
	}
}

// TestSecretsCRUD_CreateListPatchDelete walks the full lifecycle in
// one test so the assertions form a coherent narrative. Splitting
// into 4 separate tests would multiply the setup/cleanup cost
// without isolating useful failure signals.
func TestSecretsCRUD_CreateListPatchDelete(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	const name = "secrets-crud-test-key"
	h.DeleteAPIKeysByName(t, name)
	t.Cleanup(func() { h.DeleteAPIKeysByName(t, name) })

	// --- Create ---
	createBody := map[string]string{
		"name":    name,
		"purpose": "api_key",
		"owner":   "platform",
	}
	resp, body := jsonReq(t, h, http.MethodPost, routes.APISecrets, createBody, harness.DevAPIKey)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: got %d, want 201; body=%s", resp.StatusCode, body)
	}
	var created struct {
		ID    string `json:"id"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if created.ID == "" {
		t.Fatal("create returned empty id")
	}
	if len(created.Value) != 64 {
		t.Errorf("auto-generated value length: got %d, want 64 hex chars", len(created.Value))
	}

	// --- List (must include the row we just created) ---
	resp, body = jsonReq(t, h, http.MethodGet, routes.APISecrets, nil, harness.DevAPIKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: got %d, want 200; body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), name) {
		t.Errorf("list response missing created secret name; body=%s", body)
	}
	// Sanity: full value must NOT appear in the list response (only the
	// preview prefix). This guards against accidentally exposing
	// credential material through the read path.
	if strings.Contains(string(body), created.Value) {
		t.Errorf("list leaked full secret value (should be masked); body=%s", body)
	}

	// --- Patch to rotating ---
	patchBody := map[string]string{"status": "rotating"}
	resp, body = jsonReq(t, h, http.MethodPatch, routes.APISecrets+"/"+created.ID, patchBody, harness.DevAPIKey)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("patch→rotating: got %d, want 204; body=%s", resp.StatusCode, body)
	}

	// --- Patch to revoked (required before delete) ---
	patchBody = map[string]string{"status": "revoked"}
	resp, body = jsonReq(t, h, http.MethodPatch, routes.APISecrets+"/"+created.ID, patchBody, harness.DevAPIKey)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("patch→revoked: got %d, want 204; body=%s", resp.StatusCode, body)
	}

	// --- Delete ---
	resp, body = jsonReq(t, h, http.MethodDelete, routes.APISecrets+"/"+created.ID, nil, harness.DevAPIKey)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: got %d, want 204; body=%s", resp.StatusCode, body)
	}

	// --- Delete again should 404 (already gone) ---
	resp, _ = jsonReq(t, h, http.MethodDelete, routes.APISecrets+"/"+created.ID, nil, harness.DevAPIKey)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("delete-after-delete: got %d, want 404", resp.StatusCode)
	}
}

// TestSecretsCRUD_DeleteRequiresRevoked — guards the safety
// invariant that active/rotating rows can't be deleted directly.
// Operators must explicitly revoke first; this prevents accidental
// nukes of live credentials.
func TestSecretsCRUD_DeleteRequiresRevoked(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	const name = "secrets-delete-guard-test"
	h.DeleteAPIKeysByName(t, name)
	t.Cleanup(func() { h.DeleteAPIKeysByName(t, name) })

	resp, body := jsonReq(t, h, http.MethodPost, routes.APISecrets,
		map[string]string{"name": name, "purpose": "api_key", "owner": "platform"},
		harness.DevAPIKey)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: got %d", resp.StatusCode)
	}
	var created struct{ ID string }
	json.Unmarshal(body, &created)

	resp, _ = jsonReq(t, h, http.MethodDelete, routes.APISecrets+"/"+created.ID, nil, harness.DevAPIKey)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("delete active row: got %d, want 409 (must revoke first)", resp.StatusCode)
	}
}

// TestSecretsCRUD_RejectsInvalidPurpose — validation catches typos
// before they hit the CHECK constraint in Postgres (which would also
// reject, but with a 500 + leaked SQL error instead of a clean 400).
func TestSecretsCRUD_RejectsInvalidPurpose(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	resp, _ := jsonReq(t, h, http.MethodPost, routes.APISecrets,
		map[string]string{"name": "should-not-create", "purpose": "admin_key", "owner": "platform"},
		harness.DevAPIKey)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid purpose: got %d, want 400", resp.StatusCode)
	}
}

// TestSecretsCRUD_ServiceFilter — ?service=X should return only
// platform-owned + service-owned rows. This is the read pattern the
// Secrets sub-tab uses when an operator picks a service from the
// picker.
func TestSecretsCRUD_ServiceFilter(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	const platformName = "secrets-filter-test-platform"
	const dspName = "secrets-filter-test-dsp"
	const sspName = "secrets-filter-test-ssp"
	h.DeleteAPIKeysByName(t, platformName)
	h.DeleteAPIKeysByName(t, dspName)
	h.DeleteAPIKeysByName(t, sspName)
	t.Cleanup(func() {
		h.DeleteAPIKeysByName(t, platformName)
		h.DeleteAPIKeysByName(t, dspName)
		h.DeleteAPIKeysByName(t, sspName)
	})

	create := func(name, owner string) {
		resp, body := jsonReq(t, h, http.MethodPost, routes.APISecrets,
			map[string]string{"name": name, "purpose": "api_key", "owner": owner},
			harness.DevAPIKey)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create %s/%s: got %d; body=%s", name, owner, resp.StatusCode, body)
		}
	}
	create(platformName, "platform")
	create(dspName, "dsp")
	create(sspName, "ssp")

	// ?service=dsp must include platform + dsp, exclude ssp.
	resp, body := jsonReq(t, h, http.MethodGet, routes.APISecrets+"?service=dsp", nil, harness.DevAPIKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("filtered list: got %d", resp.StatusCode)
	}
	s := string(body)
	if !strings.Contains(s, platformName) {
		t.Errorf("filtered list missing platform-owned row")
	}
	if !strings.Contains(s, dspName) {
		t.Errorf("filtered list missing dsp-owned row")
	}
	if strings.Contains(s, sspName) {
		t.Errorf("filtered list leaked ssp-owned row (owner filter not applied)")
	}
}
