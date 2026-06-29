//go:build e2e

package harness

import (
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

// PublisherLineItem is the test-side projection of a publisher_line_items
// row (migration 026). Carries the IDs so downstream helpers can pause /
// resume, change pace, etc.
type PublisherLineItem struct {
	ID           string
	ExternalID   string
	PublisherID  string
	AccountID    string
	CreativeID   string
	PriorityTier string
}

// PubLineItemOpts is the param bag for AddPublisherLineItem. Zero values
// translate to sensible defaults:
//   - empty Placements: applies to every placement under the publisher
//   - zero ImpressionsCommitted: no pacing commitment
//   - empty DeliveryStart/End: open-ended flight (always in window)
//   - empty PacingMode: "even"
//   - empty Status: "active"
type PubLineItemOpts struct {
	PriorityTier         string // "sponsorship" | "guaranteed" | "house"
	DemandSource         string
	Placements           []Placement
	ImpressionsCommitted int64
	DeliveryStart        time.Time
	DeliveryEnd          time.Time
	CPM                  float64
	PacingMode           string
	Status               string
	CreativeHTML         string
}

// AddPublisherLineItem inserts a publisher_line_items row + a paired creative
// + the link table entry under the publisher's tenant context. Returns the
// derived UUIDs so the test can assert against them.
//
// Mirrors what cmd/seed/direct_sold.go does at boot, so the publisher-adserver
// warm cache picks it up identically after RefreshAllCaches.
func (h *Harness) AddPublisherLineItem(t *testing.T, pub Publisher, externalKey string, opts PubLineItemOpts) PublisherLineItem {
	t.Helper()
	if opts.PriorityTier == "" {
		t.Fatalf("AddPublisherLineItem: PriorityTier required")
	}

	lineItemID := idgen.Derive("publisher_line_item", externalKey)
	creativeID := idgen.Derive("creative", externalKey+"-creative")

	placementUUIDs := make([]string, 0, len(opts.Placements))
	for _, pl := range opts.Placements {
		placementUUIDs = append(placementUUIDs, pl.ID)
	}

	var deliveryStart, deliveryEnd sql.NullTime
	if !opts.DeliveryStart.IsZero() {
		deliveryStart = sql.NullTime{Time: opts.DeliveryStart, Valid: true}
	}
	if !opts.DeliveryEnd.IsZero() {
		deliveryEnd = sql.NullTime{Time: opts.DeliveryEnd, Valid: true}
	}

	pacing := opts.PacingMode
	if pacing == "" {
		pacing = "even"
	}
	status := opts.Status
	if status == "" {
		status = "active"
	}
	html := opts.CreativeHTML
	if html == "" {
		// Marker the test can grep for to confirm the right creative landed.
		html = `<div data-e2e-pubad="` + externalKey + `">${IMP_PIXEL}</div>`
	}

	h.WithTenant(t, pub.AccountID, func(tx *sql.Tx) {
		const crQ = `
INSERT INTO creatives (
  id, account_id, name, format, width, height, landing_url, html_content, review_status, created_at, updated_at
) VALUES ($1, $2, $3, 'display', 0, 0, '', $4, 'approved', now(), now())
ON CONFLICT (id) DO UPDATE SET html_content = EXCLUDED.html_content, review_status = 'approved', updated_at = now()`
		if _, err := tx.Exec(crQ, creativeID, pub.AccountID, externalKey, html); err != nil {
			t.Fatalf("creative upsert: %v", err)
		}

		const liQ = `
INSERT INTO publisher_line_items (
  id, account_id, publisher_id, name, demand_source, priority_tier,
  placement_ids, impressions_committed, delivery_start, delivery_end,
  cpm, currency, pacing_mode, status, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7::uuid[], $8, $9, $10, $11, 'USD', $12, $13, now(), now())
ON CONFLICT (id) DO UPDATE SET
  priority_tier = EXCLUDED.priority_tier,
  placement_ids = EXCLUDED.placement_ids,
  impressions_committed = EXCLUDED.impressions_committed,
  delivery_start = EXCLUDED.delivery_start,
  delivery_end = EXCLUDED.delivery_end,
  cpm = EXCLUDED.cpm,
  pacing_mode = EXCLUDED.pacing_mode,
  status = EXCLUDED.status,
  updated_at = now()`
		if _, err := tx.Exec(liQ,
			lineItemID, pub.AccountID, pub.ID,
			externalKey, opts.DemandSource, opts.PriorityTier,
			pq.StringArray(placementUUIDs), opts.ImpressionsCommitted,
			deliveryStart, deliveryEnd,
			opts.CPM, pacing, status,
		); err != nil {
			t.Fatalf("publisher_line_items insert: %v", err)
		}

		const linkQ = `
INSERT INTO publisher_line_item_creatives (publisher_line_item_id, creative_id, weight)
VALUES ($1, $2, 1)
ON CONFLICT (publisher_line_item_id, creative_id) DO NOTHING`
		if _, err := tx.Exec(linkQ, lineItemID, creativeID); err != nil {
			t.Fatalf("publisher_line_item_creatives link: %v", err)
		}
	})

	return PublisherLineItem{
		ID: lineItemID, ExternalID: externalKey,
		PublisherID: pub.ID, AccountID: pub.AccountID,
		CreativeID: creativeID, PriorityTier: opts.PriorityTier,
	}
}

// SetPublisherLineItemStatus updates a row's status (active / paused / ended)
// under the publisher tenant. Tests use this to demonstrate that a paused
// direct line item falls out of arbitration. RefreshAllCaches afterwards.
func (h *Harness) SetPublisherLineItemStatus(t *testing.T, li PublisherLineItem, status string) {
	t.Helper()
	h.WithTenant(t, li.AccountID, func(tx *sql.Tx) {
		if _, err := tx.Exec(`UPDATE publisher_line_items SET status = $1, updated_at = now() WHERE id = $2`,
			status, li.ID); err != nil {
			t.Fatalf("update publisher_line_items.status: %v", err)
		}
	})
}

// PubAdServeResponse is the test-side projection of the JSON the
// publisher-adserver returns. Combines the direct-win shape (source,
// line_item_id, …) and the SSP-pass-through shape (html, impression_url, …).
// Each test asserts on the fields it cares about.
type PubAdServeResponse struct {
	TraceID        string  `json:"trace_id"`
	Source         string  `json:"source"`
	LineItemID     string  `json:"line_item_id,omitempty"`
	PriorityTier   string  `json:"priority_tier,omitempty"`
	DemandSource   string  `json:"demand_source,omitempty"`
	HTML           string  `json:"html"`
	ImpressionURL  string  `json:"impression_url,omitempty"`
	ClickURL       string  `json:"click_url,omitempty"`
	ViewabilityURL string  `json:"viewability_url,omitempty"`
	Width          int     `json:"width,omitempty"`
	Height         int     `json:"height,omitempty"`
	CPM            float64 `json:"cpm,omitempty"`
	NoBid          bool    `json:"nobid,omitempty"`
	// External-Prebid-win fields. Populated when the publisher-adserver
	// fanned out to an external Prebid Server and that bid beat the SSP.
	PrebidEndpoint string  `json:"prebid_endpoint,omitempty"`
	Seat           string  `json:"seat,omitempty"`
	ClearingPrice  float64 `json:"clearing_price,omitempty"`
	Currency       string  `json:"currency,omitempty"`
	DealID         string  `json:"deal_id,omitempty"`
}

// ServePubAd calls GET /v1/pubad/serve?placement_id=... on the
// publisher-adserver. Returns the decoded response.
func (h *Harness) ServePubAd(t *testing.T, placementExternalKey string) PubAdServeResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := h.URLs.PublisherAdServer + routes.PublisherAdServe + "?placement_id=" + placementExternalKey
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build pubad request: %v", err)
	}
	resp, err := h.HTTP.Do(req)
	if err != nil {
		t.Fatalf("call pubad: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pubad status %d: %s", resp.StatusCode, string(body))
	}
	var out PubAdServeResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode pubad response: %v\nbody: %s", err, string(body))
	}
	return out
}

// ServePubAdRaw is the lower-level variant: caller supplies the full query
// string (e.g. "placement_id=pl-x&geo=USA&device=desktop"). Used by tests
// that need to control geo/device beyond what ServePubAd exposes.
func (h *Harness) ServePubAdRaw(t *testing.T, query string) PubAdServeResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := h.URLs.PublisherAdServer + routes.PublisherAdServe + "?" + query
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build pubad request: %v", err)
	}
	resp, err := h.HTTP.Do(req)
	if err != nil {
		t.Fatalf("call pubad: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pubad status %d: %s", resp.StatusCode, string(body))
	}
	var out PubAdServeResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode pubad response: %v\nbody: %s", err, string(body))
	}
	return out
}
