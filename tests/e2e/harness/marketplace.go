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

// MarketplacePurchase buys a listing as the account. Returns (status, body).
func (h *Harness) MarketplacePurchase(t *testing.T, accountID, listingID string) (int, string) {
	t.Helper()
	client := h.marketplaceClient(t, accountID)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	url := h.URLs.Gateway + routes.APIMarketplaceListingsSub + listingID + "/purchase"
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("marketplace purchase: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// MarketplaceGrants fetches the caller's grants (scope="" = purchases,
// "sales" = seller's sales).
func (h *Harness) MarketplaceGrants(t *testing.T, accountID, scope string) []marketplace.Grant {
	t.Helper()
	client := h.marketplaceClient(t, accountID)
	url := h.URLs.Gateway + routes.APIMarketplaceGrants
	if scope != "" {
		url += "?scope=" + scope
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("marketplace grants: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("marketplace grants status %d: %s", resp.StatusCode, string(b))
	}
	var out []marketplace.Grant
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode grants: %v (body=%s)", err, string(b))
	}
	return out
}

// FindListingID returns the id of the catalog listing with the given name, as
// seen by the account (empty if not found).
func (h *Harness) FindListingID(t *testing.T, accountID, name string) string {
	for _, l := range h.MarketplaceCatalog(t, accountID, "") {
		if l.Name == name {
			return l.ID
		}
	}
	return ""
}

// MarketplaceEstimate is the expansion-estimate response (clean-room-lite).
type MarketplaceEstimate struct {
	ListingName       string   `json:"listing_name"`
	YourAudienceSize  int      `json:"your_audience_size"`
	ListingSize       int      `json:"listing_size"`
	Overlap           *int     `json:"overlap"`
	OverlapPct        *float64 `json:"overlap_pct"`
	OverlapSuppressed bool     `json:"overlap_suppressed"`
	NewReachableUsers int      `json:"new_reachable_users"`
	ExpansionFactor   float64  `json:"expansion_factor"`
	MinAggregation    int      `json:"min_aggregation"`
}

// MarketplaceEstimateFor requests an expansion estimate of the account's own
// segment against a listing.
func (h *Harness) MarketplaceEstimateFor(t *testing.T, accountID, listingID, myAudienceID string) MarketplaceEstimate {
	t.Helper()
	client := h.marketplaceClient(t, accountID)
	body, _ := json.Marshal(map[string]string{"my_audience_id": myAudienceID})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	url := h.URLs.Gateway + routes.APIMarketplaceListingsSub + listingID + "/estimate"
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("marketplace estimate: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("marketplace estimate status %d: %s", resp.StatusCode, string(b))
	}
	var out MarketplaceEstimate
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode estimate: %v (body=%s)", err, string(b))
	}
	return out
}
