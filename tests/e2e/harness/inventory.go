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
	return Publisher{ID: id, ExternalID: externalKey, AccountID: owner.ID, Domain: domain}
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
