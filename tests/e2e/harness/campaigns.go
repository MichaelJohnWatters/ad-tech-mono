//go:build e2e

package harness

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/idgen"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/lib/pq"
)

// InsertionOrder is the test-side projection of an insertion_orders row.
type InsertionOrder struct {
	ID         string
	ExternalID string
	AccountID  string
	Budget     float64
}

// Campaign is the test-side projection of a line_items row (+ targeting_rules
// + line_item_creatives where set). Carries the IDs so downstream helpers
// can pause/resume, change budget, or attach more creatives.
type Campaign struct {
	ID          string
	ExternalID  string
	AccountID   string
	IOId        string
	CreativeID  string
	DailyBudget float64
	Status      string
}

// Targeting is the subset of targeting fields the e2e suite cares about.
// Extend as scenarios grow.
type Targeting struct {
	Geos       []string
	Devices    []string
	Categories []string // include_categories — a hard content filter on display/video, a soft relevance signal on retail
	Channels   []string // include_channels — per-campaign channel allowlist (empty = all channels)
}

// CreateInsertionOrder creates an IO under the given advertiser account.
func (h *Harness) CreateInsertionOrder(t *testing.T, owner Account, externalKey string, budget float64) InsertionOrder {
	t.Helper()
	if owner.Type != "advertiser" {
		t.Fatalf("CreateInsertionOrder: owner must be an advertiser account")
	}
	id := idgen.Derive("io", externalKey)

	h.WithTenant(t, owner.ID, func(tx *sql.Tx) {
		const q = `
INSERT INTO insertion_orders (id, account_id, name, budget, daily_budget, currency, start_date, end_date, status, created_at, updated_at)
VALUES ($1, $2, $3, $4, $4, 'USD', current_date, current_date + interval '90 days', 'active', now(), now())
ON CONFLICT (id) DO UPDATE SET budget = EXCLUDED.budget, updated_at = now()`
		if _, err := tx.Exec(q, id, owner.ID, externalKey, budget); err != nil {
			t.Fatalf("insertion_orders insert: %v", err)
		}
	})
	return InsertionOrder{ID: id, ExternalID: externalKey, AccountID: owner.ID, Budget: budget}
}

// CreateCampaign creates a line_item + targeting_rules + creative + link
// — the full bid-eligible bundle. Mirrors what the seed does for each YAML
// campaign so the DSP warm cache picks it up identically.
func (h *Harness) CreateCampaign(t *testing.T, owner Account, io InsertionOrder, externalKey string, baseBid, dailyBudget float64, creativeExternalKey, creativeDomain string, targeting Targeting) Campaign {
	t.Helper()
	if owner.Type != "advertiser" {
		t.Fatalf("CreateCampaign: owner must be an advertiser account")
	}
	lineItemID := idgen.Derive("line_item", externalKey)
	creativeID := idgen.Derive("creative", creativeExternalKey)
	targetingID := idgen.Derive("targeting", externalKey)

	h.WithTenant(t, owner.ID, func(tx *sql.Tx) {
		const liQ = `
INSERT INTO line_items (id, account_id, insertion_order_id, name, status, format, bid_strategy, base_bid, bid_currency, daily_budget, pacing_mode, shading_mode, creative_rotation, timezone, created_at, updated_at)
VALUES ($1, $2, $3, $4, 'live', 'display', 'cpm', $5, 'USD', $6, 'asap', 'moderate', 'bandit', 'UTC', now(), now())
ON CONFLICT (id) DO UPDATE SET status = EXCLUDED.status, base_bid = EXCLUDED.base_bid, daily_budget = EXCLUDED.daily_budget, updated_at = now()`
		if _, err := tx.Exec(liQ, lineItemID, owner.ID, io.ID, externalKey, baseBid, dailyBudget); err != nil {
			t.Fatalf("line_items insert: %v", err)
		}

		const trQ = `
INSERT INTO targeting_rules (id, line_item_id, account_id, include_geo, include_device, include_categories, include_channels, bid_modifiers, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, '{}', now(), now())
ON CONFLICT (line_item_id) DO UPDATE SET include_geo = EXCLUDED.include_geo, include_device = EXCLUDED.include_device, include_categories = EXCLUDED.include_categories, include_channels = EXCLUDED.include_channels, updated_at = now()`
		if _, err := tx.Exec(trQ, targetingID, lineItemID, owner.ID, pq.StringArray(targeting.Geos), pq.StringArray(targeting.Devices), pq.StringArray(targeting.Categories), pq.StringArray(targeting.Channels)); err != nil {
			t.Fatalf("targeting_rules insert: %v", err)
		}

		const crQ = `
INSERT INTO creatives (id, account_id, name, format, width, height, landing_url, html_content, review_status, created_at, updated_at)
VALUES ($1, $2, $3, 'display', 300, 250, $4, $5, 'approved', now(), now())
ON CONFLICT (id) DO UPDATE SET landing_url = EXCLUDED.landing_url, html_content = EXCLUDED.html_content, updated_at = now()`
		html := `<div style="width:${WIDTH}px;height:${HEIGHT}px;background:#fafafa">${CAMPAIGN_ID} via e2e</div>`
		landing := "https://" + creativeDomain
		if _, err := tx.Exec(crQ, creativeID, owner.ID, creativeExternalKey, landing, html); err != nil {
			t.Fatalf("creatives insert: %v", err)
		}

		const linkQ = `
INSERT INTO line_item_creatives (line_item_id, creative_id, weight) VALUES ($1, $2, 100)
ON CONFLICT (line_item_id, creative_id) DO NOTHING`
		if _, err := tx.Exec(linkQ, lineItemID, creativeID); err != nil {
			t.Fatalf("line_item_creatives insert: %v", err)
		}
	})

	return Campaign{
		ID: lineItemID, ExternalID: externalKey,
		AccountID: owner.ID, IOId: io.ID, CreativeID: creativeID,
		DailyBudget: dailyBudget, Status: "live",
	}
}

// CreateVideoCampaign is CreateCampaign's video sibling: a live video line item
// with a video creative (format='video', asset_url + duration_seconds) so a VAST
// serve can fill. The DSP video match gates on duration ∈ the placement's
// [min,max] window and a non-empty MediaURL (asset_url), so durationSec must sit
// inside the AddVideoPlacement window. Empty targeting = match all.
func (h *Harness) CreateVideoCampaign(t *testing.T, owner Account, io InsertionOrder, externalKey string, baseBid, dailyBudget float64, creativeExternalKey, creativeDomain string, durationSec int, targeting Targeting) Campaign {
	t.Helper()
	if owner.Type != "advertiser" {
		t.Fatalf("CreateVideoCampaign: owner must be an advertiser account")
	}
	lineItemID := idgen.Derive("line_item", externalKey)
	creativeID := idgen.Derive("creative", creativeExternalKey)
	targetingID := idgen.Derive("targeting", externalKey)

	h.WithTenant(t, owner.ID, func(tx *sql.Tx) {
		const liQ = `
INSERT INTO line_items (id, account_id, insertion_order_id, name, status, format, bid_strategy, base_bid, bid_currency, daily_budget, pacing_mode, shading_mode, creative_rotation, timezone, created_at, updated_at)
VALUES ($1, $2, $3, $4, 'live', 'video', 'cpm', $5, 'USD', $6, 'asap', 'moderate', 'bandit', 'UTC', now(), now())
ON CONFLICT (id) DO UPDATE SET status = EXCLUDED.status, format = 'video', base_bid = EXCLUDED.base_bid, daily_budget = EXCLUDED.daily_budget, updated_at = now()`
		if _, err := tx.Exec(liQ, lineItemID, owner.ID, io.ID, externalKey, baseBid, dailyBudget); err != nil {
			t.Fatalf("video line_items insert: %v", err)
		}

		const trQ = `
INSERT INTO targeting_rules (id, line_item_id, account_id, include_geo, include_device, bid_modifiers, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, '{}', now(), now())
ON CONFLICT (line_item_id) DO UPDATE SET include_geo = EXCLUDED.include_geo, include_device = EXCLUDED.include_device, updated_at = now()`
		if _, err := tx.Exec(trQ, targetingID, lineItemID, owner.ID, pq.StringArray(targeting.Geos), pq.StringArray(targeting.Devices)); err != nil {
			t.Fatalf("video targeting_rules insert: %v", err)
		}

		const crQ = `
INSERT INTO creatives (id, account_id, name, format, width, height, duration_seconds, asset_url, landing_url, review_status, created_at, updated_at)
VALUES ($1, $2, $3, 'video', 640, 360, $4, $5, $6, 'approved', now(), now())
ON CONFLICT (id) DO UPDATE SET asset_url = EXCLUDED.asset_url, duration_seconds = EXCLUDED.duration_seconds, landing_url = EXCLUDED.landing_url, updated_at = now()`
		assetURL := "https://cdn." + creativeDomain + "/" + creativeExternalKey + ".mp4"
		landing := "https://" + creativeDomain
		if _, err := tx.Exec(crQ, creativeID, owner.ID, creativeExternalKey, durationSec, assetURL, landing); err != nil {
			t.Fatalf("video creatives insert: %v", err)
		}

		const linkQ = `
INSERT INTO line_item_creatives (line_item_id, creative_id, weight) VALUES ($1, $2, 100)
ON CONFLICT (line_item_id, creative_id) DO NOTHING`
		if _, err := tx.Exec(linkQ, lineItemID, creativeID); err != nil {
			t.Fatalf("video line_item_creatives insert: %v", err)
		}
	})

	return Campaign{
		ID: lineItemID, ExternalID: externalKey,
		AccountID: owner.ID, IOId: io.ID, CreativeID: creativeID,
		DailyBudget: dailyBudget, Status: "live",
	}
}

// CreateDeal inserts a PG/Preferred/PMP deal between a publisher and one or
// more advertisers. Empty advertiserAccountIDs = open to all (PMPs usually
// have a non-empty list).
func (h *Harness) CreateDeal(t *testing.T, pub Publisher, externalKey, dealType string, price float64, advertiserAccountIDs []string, placementIDs []string) string {
	t.Helper()
	id := idgen.Derive("deal", externalKey)
	h.WithTenant(t, pub.AccountID, func(tx *sql.Tx) {
		const q = `
INSERT INTO deals (id, publisher_id, account_id, name, deal_type, price, price_currency, advertiser_ids, placement_ids, status, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, 'USD', $7::uuid[], $8::uuid[], 'active', now(), now())
ON CONFLICT (id) DO UPDATE SET deal_type = EXCLUDED.deal_type, price = EXCLUDED.price, updated_at = now()`
		if _, err := tx.Exec(q, id, pub.ID, pub.AccountID, externalKey, dealType, price, pq.StringArray(advertiserAccountIDs), pq.StringArray(placementIDs)); err != nil {
			t.Fatalf("deals insert: %v", err)
		}
	})
	return id
}

// PauseCampaign flips a line item to status='paused' and returns once the
// SQL commits. Use RefreshAllCaches (or PublishInvalidate) to push the new
// state to the DSP warm cache before asserting.
func (h *Harness) PauseCampaign(t *testing.T, c Campaign) {
	t.Helper()
	h.setCampaignStatus(t, c, "paused")
}

// ResumeCampaign flips a paused line item back to status='live'.
func (h *Harness) ResumeCampaign(t *testing.T, c Campaign) {
	t.Helper()
	h.setCampaignStatus(t, c, "live")
}

func (h *Harness) setCampaignStatus(t *testing.T, c Campaign, status string) {
	t.Helper()
	h.WithTenant(t, c.AccountID, func(tx *sql.Tx) {
		if _, err := tx.Exec("UPDATE line_items SET status = $1, updated_at = now() WHERE id = $2", status, c.ID); err != nil {
			t.Fatalf("set campaign status %s: %v", status, err)
		}
	})
}

// PatchCampaignStatus drives the change through the DSP management API
// (PATCH /v1/dsp/campaigns/{id}) instead of the direct-SQL setCampaignStatus
// path. Required for tests that care about the event-publish side-effect
// (campaign.state_changed) — direct SQL bypasses the handler and the
// publish call.
func (h *Harness) PatchCampaignStatus(t *testing.T, c Campaign, status string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"status": status})
	url := h.URLs.DSP + routes.DSPCampaigns + "/" + c.ID
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build PATCH: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", DevAPIKey)
	resp, err := h.HTTP.Do(req)
	if err != nil {
		t.Fatalf("PATCH %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("PATCH status = %d (want 204); body=%s", resp.StatusCode, string(respBody))
	}
}

// PatchProductCategory sets a line item's product_category through the DSP
// management API (PATCH /v1/dsp/campaigns/{id}), exercising the real update path.
func (h *Harness) PatchProductCategory(t *testing.T, c Campaign, cat string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"product_category": cat})
	url := h.URLs.DSP + routes.DSPCampaigns + "/" + c.ID
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build PATCH: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", DevAPIKey)
	resp, err := h.HTTP.Do(req)
	if err != nil {
		t.Fatalf("PATCH %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("PATCH product_category status = %d (want 204); body=%s", resp.StatusCode, string(b))
	}
}

// SetCampaignDailyBudget updates the daily budget on a line item under the
// owning tenant's RLS context.
func (h *Harness) SetCampaignDailyBudget(t *testing.T, c Campaign, budget float64) {
	t.Helper()
	h.WithTenant(t, c.AccountID, func(tx *sql.Tx) {
		if _, err := tx.Exec("UPDATE line_items SET daily_budget = $1, updated_at = now() WHERE id = $2", budget, c.ID); err != nil {
			t.Fatalf("set campaign daily_budget: %v", err)
		}
	})
}

// OverbidCompetitors is a base_bid value high enough that the test
// campaign reliably wins against the competitor DSPs in profiles/dsps/
// competitor{1,2}.yaml after their bid modifiers and ±30-40% noise are
// applied. Their worst-case effective bid for GBR/mobile is ~7 (c1-001
// at 3.50 × 1.30 device × 1.15 geo × 1.30 noise ≈ 6.8); 50 is well clear.
//
// Tests that need our campaign on the winning bid (e.g. to assert DealID
// or clearing price) should pass this rather than re-deriving the number.
// If you ever raise competitor noise_pct or add a higher-bidding profile,
// bump this constant — every deal/preempt test reads from here.
const OverbidCompetitors = 50.0

// SetCampaignBaseBid lifts (or lowers) a campaign's base_bid. Used by
// deal/preempt tests to push the test campaign clearly above the
// competitor DSPs' noise range so the assertion isn't flaky. See
// OverbidCompetitors for the safe overbid value.
// SetCampaignBidStrategy flips a campaign between cpm / cpc / cpa / vcpm / cpcv.
// Used by the billing-models tests to exercise reserve/settle on CPC + CPA
// without standing up an entirely separate campaign-create flow per model.
// Callers must trigger a DSP cache refresh after (RefreshAllCaches) so the
// new bid_strategy makes it into the warm cache before the next auction.
func (h *Harness) SetCampaignBidStrategy(t *testing.T, c Campaign, strategy string) {
	t.Helper()
	h.WithTenant(t, c.AccountID, func(tx *sql.Tx) {
		const q = `UPDATE line_items SET bid_strategy = $1, updated_at = now() WHERE id = $2`
		if _, err := tx.Exec(q, strategy, c.ID); err != nil {
			t.Fatalf("set bid strategy: %v", err)
		}
	})
}

// SetCampaignAttributionConfig sets the per-line-item attribution override JSON
// on the campaign's targeting_rules row (gap G5). Pass e.g.
// `{"view_window_hours":0}` to tighten the view-through window for this campaign.
func (h *Harness) SetCampaignAttributionConfig(t *testing.T, c Campaign, jsonConfig string) {
	t.Helper()
	h.WithTenant(t, c.AccountID, func(tx *sql.Tx) {
		const q = `UPDATE targeting_rules SET attribution_config = $1::jsonb, updated_at = now() WHERE line_item_id = $2`
		res, err := tx.Exec(q, jsonConfig, c.ID)
		if err != nil {
			t.Fatalf("set attribution config: %v", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			t.Fatalf("no targeting_rules row for campaign %s", c.ID)
		}
	})
}

func (h *Harness) SetCampaignBaseBid(t *testing.T, c Campaign, baseBid float64) {
	t.Helper()
	h.WithTenant(t, c.AccountID, func(tx *sql.Tx) {
		if _, err := tx.Exec("UPDATE line_items SET base_bid = $1, updated_at = now() WHERE id = $2", baseBid, c.ID); err != nil {
			t.Fatalf("set campaign base_bid: %v", err)
		}
	})
}
