//go:build e2e

package harness

import (
	"database/sql"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
)

// SetPublisherContract rewrites a publisher's revshare_model + revshare_config
// (the JSONB the billing ContractLoader decodes into a billing.Contract) and
// invalidates the reporting billing-rates cache so the change takes effect on
// the next settle. Use it to exercise tiered / guaranteed-minimum / deal-type
// contracts that the default fixed-20% fixture doesn't cover.
//
// configJSON is the raw revshare_config object, e.g.
//
//	`{"fee_pct":20,"guaranteed_min_cpm":5.0}`
//	`{"fee_pct":20,"deal_type_modifiers":{"pmp":-5}}`
//	`{"tiers":[{"min_impressions":0,"max_impressions":100,"fee_pct":30},{"min_impressions":100,"fee_pct":15}]}`
//
// Callers should RefreshAllCaches (or PublishInvalidate on the billing-rates
// subject) afterwards for the reporting contract cache to reload before the
// asserting auction/impression.
func (h *Harness) SetPublisherContract(t *testing.T, pub Publisher, model, configJSON string) {
	t.Helper()
	h.WithTenant(t, pub.AccountID, func(tx *sql.Tx) {
		const q = `
UPDATE publishers
   SET revshare_model = $2, revshare_config = $3::jsonb, updated_at = now()
 WHERE id = $1`
		res, err := tx.Exec(q, pub.ID, model, configJSON)
		if err != nil {
			t.Fatalf("SetPublisherContract(%s): %v", pub.ExternalID, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			t.Fatalf("SetPublisherContract(%s): no publisher row updated", pub.ExternalID)
		}
	})
	// Nudge the reporting contract cache so a settle right after doesn't use the
	// stale contract; RefreshAllCaches also covers this synchronously.
	h.PublishInvalidate(t, events.SubjectCacheInvalidateBillingRates)
}
