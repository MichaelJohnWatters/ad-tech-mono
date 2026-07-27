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
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestRLSIsolation(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.Reset(t)

	// RLS only bites under a NOBYPASSRLS role. h.DB connects as the owner
	// (superuser, BYPASSRLS) — correct for seeding/asserting, but it bypasses
	// the policies, so this test opens its OWN connection as the limited
	// adtech_app role (security #77, migration 067/069) and runs the
	// tenant-scoped queries through THAT. Skip (don't fail) if the role isn't
	// present, so the suite still runs on a pre-#77 database.
	appDB := openAppRoleDB(t, h)
	if appDB == nil {
		t.Skip("adtech_app role not available; skipping RLS enforcement test")
	}
	defer appDB.Close()

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
		count := countRowsAsTenant(t, appDB, advA.ID,
			"SELECT count(*) FROM line_items WHERE id = $1", campB.ID)
		if count != 0 {
			t.Errorf("tenant A saw B's line_item (count=%d); RLS policy not blocking", count)
		}
	})

	t.Run("B_cannot_see_A_line_items", func(t *testing.T) {
		count := countRowsAsTenant(t, appDB, advB.ID,
			"SELECT count(*) FROM line_items WHERE id = $1", campA.ID)
		if count != 0 {
			t.Errorf("tenant B saw A's line_item (count=%d); RLS policy not blocking", count)
		}
	})

	t.Run("A_cannot_see_B_targeting_rules", func(t *testing.T) {
		count := countRowsAsTenant(t, appDB, advA.ID,
			"SELECT count(*) FROM targeting_rules WHERE line_item_id = $1", campB.ID)
		if count != 0 {
			t.Errorf("tenant A saw B's targeting_rules (count=%d)", count)
		}
	})

	t.Run("A_cannot_see_B_creatives", func(t *testing.T) {
		count := countRowsAsTenant(t, appDB, advA.ID,
			"SELECT count(*) FROM creatives WHERE id = $1", campB.CreativeID)
		if count != 0 {
			t.Errorf("tenant A saw B's creative (count=%d)", count)
		}
	})

	t.Run("A_cannot_insert_under_B_account_id", func(t *testing.T) {
		// Explicitly attempt to write a line_item with account_id = B while
		// the session is scoped to A. RLS must block this — the policy uses
		// USING which applies to both reads and writes.
		err := tryInsertAsTenant(t, appDB, advA.ID,
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
		count := countRowsAsTenant(t, appDB, advA.ID,
			"SELECT count(*) FROM line_items WHERE id = $1", campA.ID)
		if count != 1 {
			t.Errorf("tenant A could not see own line_item (count=%d); RLS blocking too much", count)
		}
	})
}

// openAppRoleDB opens a connection as the limited adtech_app (NOBYPASSRLS)
// role, derived from the harness owner URL by swapping in the app credentials.
// Override the whole URL with E2E_APP_POSTGRES_URL. Returns nil (→ the caller
// skips) if the role can't be reached — e.g. a pre-#77 database.
func openAppRoleDB(t *testing.T, h *harness.Harness) *sql.DB {
	t.Helper()
	appURL := os.Getenv("E2E_APP_POSTGRES_URL")
	if appURL == "" {
		u, err := url.Parse(h.URLs.PostgresURL)
		if err != nil {
			t.Fatalf("parse owner postgres URL: %v", err)
		}
		// Dev default matches migration 069 / values.yaml. Prod supplies the
		// real credentials via E2E_APP_POSTGRES_URL.
		u.User = url.UserPassword("adtech_app", "adtech-app-local")
		appURL = u.String()
	}
	db, err := sql.Open("postgres", appURL)
	if err != nil {
		t.Logf("open adtech_app connection: %v", err)
		return nil
	}
	if err := db.Ping(); err != nil {
		t.Logf("ping adtech_app connection: %v", err)
		db.Close()
		return nil
	}
	return db
}

// countRowsAsTenant runs a tenant-scoped SELECT through the app-role connection
// (a tx + SET LOCAL like the production code does) and returns the count.
func countRowsAsTenant(t *testing.T, db *sql.DB, accountID, query string, args ...any) int {
	t.Helper()
	var n int
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("SELECT set_config('app.current_account_id', $1, true)", accountID); err != nil {
		t.Fatalf("set tenant: %v", err)
	}
	if err := tx.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count query: %v", err)
	}
	return n
}

// tryInsertAsTenant runs an INSERT under accountID (through the app-role
// connection) and returns the error — nil means the insert succeeded, which
// the caller treats as a failure when RLS was supposed to block it.
func tryInsertAsTenant(t *testing.T, db *sql.DB, accountID, query string, args ...any) (insertErr error) {
	t.Helper()
	tx, err := db.Begin()
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
