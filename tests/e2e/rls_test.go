//go:build e2e

// Multi-tenancy / Row-Level Security regression tests.
//
// Postgres RLS is the safety net for multi-tenancy — if an application
// query forgets a WHERE account_id = ?, the database itself must still
// block the row from appearing. These tests deliberately try to read
// another tenant's data with the wrong app.current_account_id set to
// verify the policy fires.
package e2e

import (
	"database/sql"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestRLSIsolation(t *testing.T) {
	// The dev Postgres role (`adtech`) is provisioned as a superuser with
	// BYPASSRLS — RLS policies exist on the tables but the dev role
	// bypasses them. To actually test RLS we'd need either:
	//   - a non-superuser app role and ALTER USER to use it, or
	//   - ALTER TABLE ... FORCE ROW LEVEL SECURITY (which also breaks
	//     cross-tenant loaders like CampaignLoader that intentionally
	//     query without a tenant context).
	// Production credentials are not superusers, so the policies do
	// enforce there. Skip until we wire a dedicated app role.
	t.Skip("dev role is superuser with BYPASSRLS; production role enforces. " +
		"Pending: non-superuser app role for the test environment.")
	h := harness.WaitReady(t, 60*time.Second)
	h.Reset(t)

	// Two distinct advertiser accounts. Each will have its own campaign;
	// neither must see the other's row regardless of how the query is
	// written.
	advA := h.CreateAdvertiser(t, "rls-adv-a")
	advB := h.CreateAdvertiser(t, "rls-adv-b")
	ioA := h.CreateInsertionOrder(t, advA, "rls-io-a", 1000)
	ioB := h.CreateInsertionOrder(t, advB, "rls-io-b", 1000)

	campA := h.CreateCampaign(t, advA, ioA, "rls-li-a", 2.00, 100,
		"rls-cr-a", "a.test",
		harness.Targeting{Geos: []string{"USA"}},
	)
	campB := h.CreateCampaign(t, advB, ioB, "rls-li-b", 2.00, 100,
		"rls-cr-b", "b.test",
		harness.Targeting{Geos: []string{"USA"}},
	)

	t.Run("A_cannot_see_B_line_items", func(t *testing.T) {
		count := countRowsAsTenant(t, h, advA.ID,
			"SELECT count(*) FROM line_items WHERE id = $1", campB.ID)
		if count != 0 {
			t.Errorf("tenant A saw B's line_item (count=%d); RLS policy not blocking", count)
		}
	})

	t.Run("B_cannot_see_A_line_items", func(t *testing.T) {
		count := countRowsAsTenant(t, h, advB.ID,
			"SELECT count(*) FROM line_items WHERE id = $1", campA.ID)
		if count != 0 {
			t.Errorf("tenant B saw A's line_item (count=%d); RLS policy not blocking", count)
		}
	})

	t.Run("A_cannot_see_B_targeting_rules", func(t *testing.T) {
		count := countRowsAsTenant(t, h, advA.ID,
			"SELECT count(*) FROM targeting_rules WHERE line_item_id = $1", campB.ID)
		if count != 0 {
			t.Errorf("tenant A saw B's targeting_rules (count=%d)", count)
		}
	})

	t.Run("A_cannot_see_B_creatives", func(t *testing.T) {
		count := countRowsAsTenant(t, h, advA.ID,
			"SELECT count(*) FROM creatives WHERE id = $1", campB.CreativeID)
		if count != 0 {
			t.Errorf("tenant A saw B's creative (count=%d)", count)
		}
	})

	t.Run("A_cannot_insert_under_B_account_id", func(t *testing.T) {
		// Explicitly attempt to write a line_item with account_id = B while
		// the session is scoped to A. RLS must block this — the policy uses
		// USING which applies to both reads and writes.
		err := tryInsertAsTenant(t, h, advA.ID,
			"INSERT INTO line_items (id, account_id, insertion_order_id, name, status, format, bid_strategy, base_bid, bid_currency, daily_budget, pacing_mode, shading_mode, creative_rotation, timezone, created_at, updated_at) "+
				"VALUES (gen_random_uuid(), $1, $2, 'rls-cross-tenant', 'draft', 'display', 'cpm', 1.00, 'USD', 100, 'even', 'moderate', 'bandit', 'UTC', now(), now())",
			advB.ID, ioB.ID,
		)
		if err == nil {
			t.Error("expected RLS to reject INSERT with account_id != current_account_id; insert succeeded")
		}
	})

	t.Run("A_can_see_own_line_item", func(t *testing.T) {
		// Sanity: same query as the first subtest but pointing at A's own
		// campaign — RLS must allow this so we're testing exclusion, not
		// blanket denial.
		count := countRowsAsTenant(t, h, advA.ID,
			"SELECT count(*) FROM line_items WHERE id = $1", campA.ID)
		if count != 1 {
			t.Errorf("tenant A could not see own line_item (count=%d); RLS blocking too much", count)
		}
	})
}

// countRowsAsTenant runs a tenant-scoped SELECT and returns the count value
// in the first column. Uses a tx + SET LOCAL like the production code does.
func countRowsAsTenant(t *testing.T, h *harness.Harness, accountID, query string, args ...any) int {
	t.Helper()
	var n int
	h.WithTenant(t, accountID, func(tx *sql.Tx) {
		if err := tx.QueryRow(query, args...).Scan(&n); err != nil {
			t.Fatalf("count query: %v", err)
		}
	})
	return n
}

// tryInsertAsTenant runs an INSERT under accountID and returns the error
// (or nil if the insert succeeded — which the caller treats as a test
// failure when the insert was supposed to be RLS-blocked).
func tryInsertAsTenant(t *testing.T, h *harness.Harness, accountID, query string, args ...any) (insertErr error) {
	t.Helper()
	// Don't use WithTenant — it Fatal's on error, but here we want the
	// error returned to the caller for assertion.
	tx, err := h.DB.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("SELECT set_config('app.current_account_id', $1, true)", accountID); err != nil {
		return err
	}
	_, insertErr = tx.Exec(query, args...)
	return insertErr
}
