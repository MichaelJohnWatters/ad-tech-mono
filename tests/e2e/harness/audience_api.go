//go:build e2e

package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// devAPIKey is the operator key SeedDevSecrets inserts (cmd/seed). The
// gateway's secrets-backed API-key auth accepts it, so the harness can call
// operator endpoints like the audience upload without minting a key.
const devAPIKey = "dev-api-key-do-not-use-in-prod"

// UploadAudience POSTs a CRM upload to the gateway's audience endpoint and
// returns the created segment ID. Exercises the production write path (as
// opposed to the direct-SQL CreateSegment/AddUserToSegment helpers).
func (h *Harness) UploadAudience(t *testing.T, accountID, name, visibility string, userIDs []string) string {
	t.Helper()
	// The audiences endpoint is now tenant-scoped from the JWT claims (was a
	// service-key operator endpoint). Authenticate as an owner of accountID —
	// the endpoint binds the segment to the session's account and ignores any
	// body account_id, so a caller can't write another tenant's data.
	email := "aud-upload-" + accountID + "@e2e.local"
	h.CreateLoginUser(t, accountID, email, "e2e-pass", "owner")
	client := h.LoginAs(t, email, "e2e-pass")

	body, _ := json.Marshal(map[string]any{
		"name":       name,
		"visibility": visibility,
		"user_ids":   userIDs,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URLs.Gateway+routes.APIAudiences, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build audience upload: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("audience upload call: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("audience upload status %d: %s", resp.StatusCode, string(respBody))
	}
	var out struct {
		SegmentID    string `json:"segment_id"`
		MembersAdded int    `json:"members_added"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		t.Fatalf("decode audience upload response: %v", err)
	}
	return out.SegmentID
}

// SetSegmentTaxonomy labels a segment with an IAB Audience Taxonomy node via
// the gateway API (PUT /v1/api/audiences/taxonomy) — the same tenant-scoped
// write path the portal picker uses.
func (h *Harness) SetSegmentTaxonomy(t *testing.T, accountID, segmentID string, taxonomyID int64) {
	t.Helper()
	email := "aud-upload-" + accountID + "@e2e.local"
	h.CreateLoginUser(t, accountID, email, "e2e-pass", "owner")
	client := h.LoginAs(t, email, "e2e-pass")

	body, _ := json.Marshal(map[string]any{
		"segment_id":  segmentID,
		"taxonomy_id": taxonomyID,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, h.URLs.Gateway+routes.APIAudienceTaxonomy, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build taxonomy set: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("taxonomy set call: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("taxonomy set status %d: %s", resp.StatusCode, string(respBody))
	}
}

// SegmentMemberCount returns how many members a segment has (direct DB read
// for assertions).
func (h *Harness) SegmentMemberCount(t *testing.T, segmentID string) int {
	t.Helper()
	var n int
	if err := h.DB.QueryRow(`SELECT count(*) FROM audience_segment_members WHERE segment_id = $1`, segmentID).Scan(&n); err != nil {
		t.Fatalf("count segment members: %v", err)
	}
	return n
}
