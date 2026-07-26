//go:build e2e

// Data-provider registry + ingest contract (ADR 0009), end to end. This
// feature shipped "live-verified" with zero e2e coverage — the factory
// reset erased the only proof it worked. Covers: tenant-scoped CRUD,
// provenance stamping (provider_id + data_party ride the upload into the
// segment row), and the encryption contract (a cleartext file to an
// encryption_expected provider is rejected up front).
package e2e

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// createProvider POSTs a provider for the account and returns its id.
func createProvider(t *testing.T, h *harness.Harness, client *http.Client, body map[string]any) string {
	t.Helper()
	payload, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, h.URLs.Gateway+routes.APIAudienceProviders, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("build provider create: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("provider create call: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != http.StatusOK || out.ID == "" {
		t.Fatalf("provider create status %d decode err %v id %q", resp.StatusCode, err, out.ID)
	}
	return out.ID
}

func listProviders(t *testing.T, h *harness.Harness, client *http.Client) []map[string]any {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, h.URLs.Gateway+routes.APIAudienceProviders, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("provider list call: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("provider list status %d", resp.StatusCode)
	}
	var out []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func TestDataProviderRegistryAndIngestContract(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "dprov")

	email := "dprov-owner@" + w.AdvAcc.ID + ".e2e.local"
	h.CreateLoginUser(t, w.AdvAcc.ID, email, "e2e-pass-dprov", "owner")
	client := h.LoginAs(t, email, "e2e-pass-dprov")

	// CRUD: a relaxed first-party CRM feed and a licensed third-party
	// provider whose contract requires encrypted files.
	relaxedID := createProvider(t, h, client, map[string]any{
		"name": "E2E CRM Feed", "kind": "crm",
	})
	encryptedID := createProvider(t, h, client, map[string]any{
		"name": "E2E Licensed DMP", "kind": "dmp",
		"default_party": "third", "default_licence": "purchased",
		"encryption_expected": true,
	})
	if got := len(listProviders(t, h, client)); got != 2 {
		t.Fatalf("provider list = %d entries, want 2", got)
	}

	// Tenant scoping: another account's session must not see these.
	otherEmail := "dprov-other@" + w.PubAcc.ID + ".e2e.local"
	h.CreateLoginUser(t, w.PubAcc.ID, otherEmail, "e2e-pass-dprov", "owner")
	other := h.LoginAs(t, otherEmail, "e2e-pass-dprov")
	for _, p := range listProviders(t, h, other) {
		if p["id"] == relaxedID || p["id"] == encryptedID {
			t.Fatalf("tenant leak: other account sees provider %v", p["id"])
		}
	}

	// Provenance: an upload attributed to the relaxed provider stamps
	// provider_id + the provider's party classification onto the segment.
	code, body := h.UploadAudienceCSVProviderStatus(t, w.AdvAcc.ID, "dprov-seg", "public",
		"user_id\ndprov-u1\ndprov-u2\n", relaxedID)
	if code != http.StatusOK {
		t.Fatalf("relaxed-provider upload status %d: %s", code, body)
	}
	var up struct {
		SegmentID string `json:"segment_id"`
	}
	_ = json.Unmarshal([]byte(body), &up)
	if up.SegmentID == "" {
		t.Fatalf("relaxed-provider upload returned no segment_id: %s", body)
	}
	var gotProvider, gotParty string
	if err := h.DB.QueryRow(
		`SELECT COALESCE(provider_id::text,''), COALESCE(data_party,'') FROM audience_segments WHERE id = $1`,
		up.SegmentID).Scan(&gotProvider, &gotParty); err != nil {
		t.Fatalf("segment provenance query: %v", err)
	}
	if gotProvider != relaxedID {
		t.Errorf("segment provider_id = %q, want %q", gotProvider, relaxedID)
	}
	if gotParty != "first" {
		t.Errorf("segment data_party = %q, want first (crm kind default)", gotParty)
	}

	// Encryption contract: a CLEARTEXT file to the encryption_expected
	// provider is a content reject, up front, with a reason.
	code, body = h.UploadAudienceCSVProviderStatus(t, w.AdvAcc.ID, "dprov-clear", "public",
		"user_id\ndprov-u3\n", encryptedID)
	if code == http.StatusOK {
		t.Fatalf("cleartext upload to encryption_expected provider must be rejected, got 200: %s", body)
	}
	if !strings.Contains(strings.ToLower(body), "encrypt") {
		t.Errorf("reject body should mention encryption, got: %s", body)
	}
}
