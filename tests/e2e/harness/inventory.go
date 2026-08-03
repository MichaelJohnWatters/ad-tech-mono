//go:build e2e

package harness

import (
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/idgen"
)

// Publisher is the test-side projection of a publishers row.
type Publisher struct {
	ID         string
	ExternalID string
	AccountID  string // owning account (publisher type)
	Domain     string
}

// Placement is the test-side projection of a placements row.
type Placement struct {
	ID          string
	ExternalID  string
	PublisherID string
	FloorPrice  float64
	Width       int
	Height      int
}

// AddPublisher creates a publishers row owned by the given account. Returns
// the derived UUID so downstream calls (placement creation, deals) can wire
// it without another DB lookup.
func (h *Harness) AddPublisher(t *testing.T, owner Account, externalKey, domain string) Publisher {
	t.Helper()
	if owner.Type != "publisher" {
		t.Fatalf("AddPublisher: owner must be a publisher account, got %s", owner.Type)
	}
	id := idgen.Derive("publisher", externalKey)

	revShareJSON, _ := json.Marshal(map[string]any{"fee_pct": 20})
	h.WithTenant(t, owner.ID, func(tx *sql.Tx) {
		const q = `
INSERT INTO publishers (id, account_id, name, domain, currency, status, revshare_model, revshare_config, payment_terms, created_at, updated_at)
VALUES ($1, $2, $3, $4, 'USD', 'active', 'fixed', $5, 'net_30', now(), now())
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, domain = EXCLUDED.domain, updated_at = now()`
		if _, err := tx.Exec(q, id, owner.ID, externalKey, domain, revShareJSON); err != nil {
			t.Fatalf("publishers insert: %v", err)
		}
	})
	// Authorise this publisher's domain to sell through our exchange, so the
	// world serves under the prod-shaped strict ads.txt enforcement (values:
	// exchange.adstxt_enforcement=strict). Without this every auction from a
	// harness-created publisher no-bids "adstxt_not_authorised". The exchange's
	// warm ads.txt cache picks it up on the next RefreshAllCaches (world builders
	// call it after inventory setup). ads_txt_cache is a global (non-RLS) table.
	h.authorizeAdsTxt(t, domain)
	return Publisher{ID: id, ExternalID: externalKey, AccountID: owner.ID, Domain: domain}
}

// authorizeAdsTxt writes the authorising ads.txt line (adtech.local /
// adtech-exchange / DIRECT — matching the exchange's configured seller identity)
// for a publisher domain, exactly as the cmd/adstxt crawler would after fetching
// a real ads.txt. Idempotent. Domains are unique per publisher external key, so
// this never clobbers another test's authorisation.
func (h *Harness) authorizeAdsTxt(t *testing.T, domain string) {
	t.Helper()
	if domain == "" {
		return
	}
	const entries = `[{"Domain":"adtech.local","AccountID":"adtech-exchange","Relationship":"DIRECT"}]`
	if _, err := h.DB.Exec(`
INSERT INTO ads_txt_cache (domain, entries, status, last_fetched, last_changed)
VALUES ($1, $2::jsonb, 'valid', now(), now())
ON CONFLICT (domain) DO UPDATE SET entries = EXCLUDED.entries, status = 'valid', last_fetched = now()`,
		domain, entries); err != nil {
		t.Fatalf("authorize ads.txt for %s: %v", domain, err)
	}
}

// AddPlacement attaches a placement to a publisher with the given format
// and floor price. Categories are stored in the floor_config JSONB column
// the same way the standard seed does it (so warm cache reads work).
func (h *Harness) AddPlacement(t *testing.T, pub Publisher, externalKey string, width, height int, floor float64, categories []string) Placement {
	t.Helper()
	id := idgen.Derive("placement", externalKey)
	floorJSON, _ := json.Marshal(map[string]any{"categories": categories})

	h.WithTenant(t, pub.AccountID, func(tx *sql.Tx) {
		const q = `
INSERT INTO placements (id, publisher_id, account_id, name, format, width, height, floor_price, floor_currency, page_url_pattern, status, floor_config, created_at, updated_at)
VALUES ($1, $2, $3, $4, 'display', $5, $6, $7, 'USD', $8, 'active', $9, now(), now())
ON CONFLICT (id) DO UPDATE SET floor_price = EXCLUDED.floor_price, updated_at = now()`
		pageURL := "https://" + pub.Domain + "/p/" + externalKey
		if _, err := tx.Exec(q, id, pub.ID, pub.AccountID, externalKey, width, height, floor, pageURL, floorJSON); err != nil {
			t.Fatalf("placements insert: %v", err)
		}
	})
	return Placement{
		ID: id, ExternalID: externalKey, PublisherID: pub.ID,
		FloorPrice: floor, Width: width, Height: height,
	}
}

// AddChannelPlacement is AddPlacement with an explicit channel format (dooh /
// retail / ingame / …) and a surface/slot count — the publisher-declared channel
// inventory the SSP serve reads (channel + surfaces default from the placement).
func (h *Harness) AddChannelPlacement(t *testing.T, pub Publisher, externalKey string, width, height int, floor float64, format string, surfaces int) Placement {
	t.Helper()
	id := idgen.Derive("placement", externalKey)
	floorJSON, _ := json.Marshal(map[string]any{"categories": []string{}})
	h.WithTenant(t, pub.AccountID, func(tx *sql.Tx) {
		const q = `
INSERT INTO placements (id, publisher_id, account_id, name, format, width, height, surfaces, floor_price, floor_currency, page_url_pattern, status, floor_config, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'USD', $10, 'active', $11, now(), now())
ON CONFLICT (id) DO UPDATE SET format = EXCLUDED.format, surfaces = EXCLUDED.surfaces, updated_at = now()`
		pageURL := "https://" + pub.Domain + "/p/" + externalKey
		if _, err := tx.Exec(q, id, pub.ID, pub.AccountID, externalKey, format, width, height, surfaces, floor, pageURL, floorJSON); err != nil {
			t.Fatalf("channel placement insert: %v", err)
		}
	})
	return Placement{ID: id, ExternalID: externalKey, PublisherID: pub.ID, FloorPrice: floor, Width: width, Height: height}
}

// AddVideoPlacement creates a video ad slot (format='video' + a video_config
// window) so a VAST/CTV serve can actually fill. The DSP's video creative match
// gates on the request's [minDur,maxDur] (derived from this video_config), so a
// video creative's duration_seconds must fall inside it. 640x360 pre-roll.
func (h *Harness) AddVideoPlacement(t *testing.T, pub Publisher, externalKey string, floor float64, minDur, maxDur int) Placement {
	t.Helper()
	id := idgen.Derive("placement", externalKey)
	videoJSON, _ := json.Marshal(map[string]any{
		"skippable": false, "min_duration": minDur, "max_duration": maxDur,
		"plcmt": 3, "mimes": []string{"video/mp4"},
	})
	h.WithTenant(t, pub.AccountID, func(tx *sql.Tx) {
		const q = `
INSERT INTO placements (id, publisher_id, account_id, name, format, width, height, floor_price, floor_currency, page_url_pattern, status, video_config, created_at, updated_at)
VALUES ($1, $2, $3, $4, 'video', 640, 360, $5, 'USD', $6, 'active', $7, now(), now())
ON CONFLICT (id) DO UPDATE SET floor_price = EXCLUDED.floor_price, format = 'video', video_config = EXCLUDED.video_config, updated_at = now()`
		pageURL := "https://" + pub.Domain + "/v/" + externalKey
		if _, err := tx.Exec(q, id, pub.ID, pub.AccountID, externalKey, floor, pageURL, videoJSON); err != nil {
			t.Fatalf("video placements insert: %v", err)
		}
	})
	return Placement{ID: id, ExternalID: externalKey, PublisherID: pub.ID, FloorPrice: floor, Width: 640, Height: 360}
}
