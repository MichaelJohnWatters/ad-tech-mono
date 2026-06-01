//go:build e2e

// Creative serving — verifies the ad server's resolver pathways:
//   1. html_content column → returned directly (small banners)
//   2. asset_url → fetched from Minio (large assets)  — skipped, needs upload step
//   3. unknown creative id → default fallback HTML
//   4. review_status filter → only approved creatives are servable
package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// TestCreativeHTMLContentServed — when html_content is set in the DB, the
// ad server returns it after macro substitution. BuildBasicWorld already
// inserts a creative with html_content; we just verify it lands in the
// served HTML.
func TestCreativeHTMLContentServed(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "cr-html")

	body := serveAdAndReadHTML(t, h, models.ServeRequest{
		TraceID:       "cr-html-trace",
		CampaignID:    w.Campaign.ID,
		CreativeID:    w.Campaign.CreativeID,
		PlacementID:   w.Placement.ID,
		PublisherID:   w.Publisher.ID,
		AdvertiserID:  w.AdvAcc.ID,
		ClearingPrice: 1.00,
		Currency:      "USD",
		SiteDomain:    w.Publisher.Domain,
		Width:         300,
		Height:        250,
	})

	// BuildBasicWorld inserts html_content with "${CAMPAIGN_ID} via e2e"
	// (see campaigns.go). The macro should expand to the actual campaign ID.
	if !strings.Contains(body, w.Campaign.ID) {
		t.Errorf("served HTML missing campaign id %q; body=%s", w.Campaign.ID, body)
	}
	if !strings.Contains(body, "via e2e") {
		t.Errorf("served HTML missing seeded html_content marker; body=%s", body)
	}
}

// TestCreativeUnknownIDReturnsDefault — a serve request for an unknown
// creative falls back to the ad server's inline default HTML rather than
// erroring. Tests the ad server's "creative resolver miss" path.
func TestCreativeUnknownIDReturnsDefault(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "cr-unknown")

	body := serveAdAndReadHTML(t, h, models.ServeRequest{
		TraceID:       "cr-unknown-trace",
		CampaignID:    w.Campaign.ID,
		CreativeID:    "00000000-0000-0000-0000-000000000000", // not in DB
		PlacementID:   w.Placement.ID,
		PublisherID:   w.Publisher.ID,
		AdvertiserID:  w.AdvAcc.ID,
		ClearingPrice: 1.00,
		Currency:      "USD",
		SiteDomain:    w.Publisher.Domain,
		Width:         300,
		Height:        250,
	})

	// The fallback HTML in cmd/adserver/main.go contains the literal
	// string "Advertisement" — distinct from anything BuildBasicWorld set.
	if !strings.Contains(body, "Advertisement") {
		t.Errorf("expected default fallback HTML, got %s", body)
	}
}

// TestCreativeRejectedNotInCache — a creative with review_status='rejected'
// must not appear in the ad server warm cache. Verifies the approved-only
// filter in pkg/store/postgres.CreativeLoader.
func TestCreativeRejectedNotInCache(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "cr-rejected")

	// Reject the creative under the owning advertiser's tenant context so
	// the RLS policy on creatives admits the UPDATE.
	h.WithTenant(t, w.AdvAcc.ID, func(tx *sql.Tx) {
		if _, err := tx.Exec(`UPDATE creatives SET review_status = 'rejected', updated_at = now() WHERE id = $1`,
			w.Campaign.CreativeID); err != nil {
			t.Fatalf("reject creative: %v", err)
		}
	})
	h.RefreshAllCaches(t)

	// Verify it disappeared from /v1/ad/creatives
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.URLs.AdServer+routes.AdCreatives, nil)
	resp, err := h.HTTP.Do(req)
	if err != nil {
		t.Fatalf("list creatives: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var ids []string
	_ = json.Unmarshal(body, &ids)
	for _, id := range ids {
		if id == w.Campaign.CreativeID {
			t.Errorf("rejected creative %q still in ad server cache", w.Campaign.CreativeID)
		}
	}
}

// --- helpers ---------------------------------------------------------------

func serveAdAndReadHTML(t *testing.T, h *harness.Harness, req models.ServeRequest) string {
	t.Helper()
	body, _ := json.Marshal(req)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	httpReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.URLs.AdServer+routes.AdServe, bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := h.HTTP.Do(httpReq)
	if err != nil {
		t.Fatalf("serve call: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("serve status %d: %s", resp.StatusCode, string(respBody))
	}
	var sr models.ServeResponse
	if err := json.Unmarshal(respBody, &sr); err != nil {
		t.Fatalf("serve decode: %v", err)
	}
	return sr.HTML
}
