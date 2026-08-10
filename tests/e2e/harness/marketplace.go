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

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/marketplace"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// marketplaceClient logs in a per-account user and returns an authed client.
func (h *Harness) marketplaceClient(t *testing.T, accountID string) *http.Client {
	t.Helper()
	email := "mkt-" + accountID + "@e2e.local"
	h.CreateLoginUser(t, accountID, email, "e2e-pass", "owner")
	return h.LoginAs(t, email, "e2e-pass")
}

// CreatePublicSegment inserts a PUBLIC audience segment for the account (direct
// DB, owner role) and returns its id — the sell-side input to a listing.
func (h *Harness) CreatePublicSegment(t *testing.T, accountID, name string, members []string) string {
	t.Helper()
	var segID string
	if err := h.DB.QueryRow(`
INSERT INTO audience_segments (account_id, name, type, status, source, visibility)
VALUES ($1::uuid, $2, 'first_party', 'active', 'seed', 'public') RETURNING id::text`,
		accountID, name).Scan(&segID); err != nil {
		t.Fatalf("create public segment: %v", err)
	}
	for _, m := range members {
		if _, err := h.DB.Exec(`
INSERT INTO audience_segment_members (segment_id, user_id, account_id, added_at)
VALUES ($1::uuid, $2, $3::uuid, now()) ON CONFLICT DO NOTHING`, segID, m, accountID); err != nil {
			t.Fatalf("add segment member: %v", err)
		}
	}
	return segID
}

// MarketplaceList lists a segment on the marketplace as the account. Returns
// (status, listing id or error body).
func (h *Harness) MarketplaceList(t *testing.T, accountID, segmentID, name string, cpmSurcharge float64) (int, string) {
	t.Helper()
	client := h.marketplaceClient(t, accountID)
	body, _ := json.Marshal(map[string]any{"segment_id": segmentID, "name": name, "cpm_surcharge": cpmSurcharge})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.URLs.Gateway+routes.APIMarketplaceListings, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("marketplace list: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// MarketplaceCatalog fetches the catalog (scope="" = browse, "mine" = own) as
// the account.
func (h *Harness) MarketplaceCatalog(t *testing.T, accountID, scope string) []marketplace.Listing {
	t.Helper()
	client := h.marketplaceClient(t, accountID)
	url := h.URLs.Gateway + routes.APIMarketplaceListings
	if scope != "" {
		url += "?scope=" + scope
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("marketplace catalog: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("marketplace catalog status %d: %s", resp.StatusCode, string(b))
	}
	var out []marketplace.Listing
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode catalog: %v (body=%s)", err, string(b))
	}
	return out
}

// HasListing reports whether a listing with the given name is in the slice.
func HasListing(ls []marketplace.Listing, name string) bool {
	for _, l := range ls {
		if l.Name == name {
			return true
		}
	}
	return false
}
