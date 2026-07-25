//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"

	_ "github.com/lib/pq"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// TestRLSPlatformReadHatch is the ENFORCING RLS test that tests/e2e/rls_test.go
// can't be: it runs the isolation checks as a real NOBYPASSRLS role, so the
// tenant_isolation policies actually bite (the dev `adtech` role is a superuser
// that bypasses RLS, which is why the e2e version is skipped). It proves the
// migration-065 escape hatch (security #77): a limited role sees only its tenant
// with app.current_account_id set, sees everything with app.platform_read='on'
// (how the cross-tenant warm-cache loaders will work post-downgrade), sees
// nothing with neither GUC (no error), and cannot read or write across tenants.
//
// Runs against DATABASE_URL (or the local-stack default). Skips cleanly when
// Postgres is unreachable, migration 065 hasn't been applied, or the connecting
// role can't CREATE ROLE. Everything it creates (a uniquely-named probe role +
// two throwaway tenants) is cleaned up.
func TestRLSPlatformReadHatch(t *testing.T) {
	baseURL := os.Getenv("DATABASE_URL")
	if baseURL == "" {
		baseURL = routes.DefaultPostgresURL
	}
	ctx := context.Background()

	super, err := sql.Open("postgres", baseURL)
	if err != nil {
		t.Skipf("postgres open: %v", err)
	}
	// Registered FIRST so it runs LAST (t.Cleanup is LIFO) — the teardown below
	// still needs `super` open. (A plain `defer super.Close()` would fire before
	// t.Cleanup, closing the DB out from under the teardown → silent no-op leak.)
	t.Cleanup(func() { super.Close() })
	if err := super.PingContext(ctx); err != nil {
		t.Skipf("postgres unreachable (%v) — start the local stack or set DATABASE_URL", err)
	}

	// Skip unless migration 065 is applied (policies carry the platform-read hatch).
	var hatched int
	if err := super.QueryRowContext(ctx,
		`SELECT count(*) FROM pg_policies
		 WHERE policyname = 'tenant_isolation' AND qual LIKE '%app.platform_read%'`,
	).Scan(&hatched); err != nil || hatched == 0 {
		t.Skipf("migration 065 (platform-read hatch) not applied: hatched=%d err=%v", hatched, err)
	}

	const (
		role = "rls_probe"
		pw   = "probe"
		accA = "aaaaaaaa-0000-4000-8000-0000000000aa"
		accB = "bbbbbbbb-0000-4000-8000-0000000000bb"
	)

	// Idempotent teardown helper — also run up-front so a prior interrupted run
	// (leaked role / seed rows) can't fail this one with stale state.
	cleanup := func() {
		super.ExecContext(ctx, `DELETE FROM data_providers WHERE account_id IN ($1,$2)`, accA, accB)
		super.ExecContext(ctx, `DELETE FROM advertiser_balances WHERE account_id IN ($1,$2)`, accA, accB)
		super.ExecContext(ctx, `DELETE FROM accounts WHERE id IN ($1,$2)`, accA, accB)
		super.ExecContext(ctx, `DO $$ BEGIN
			IF EXISTS (SELECT FROM pg_roles WHERE rolname = '`+role+`') THEN
				EXECUTE 'DROP OWNED BY `+role+`'; EXECUTE 'DROP ROLE `+role+`';
			END IF; END $$;`)
	}
	cleanup()
	t.Cleanup(cleanup)

	// Need CREATE ROLE (dev role is a superuser; a locked-down CI role isn't).
	if _, err := super.ExecContext(ctx,
		fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOBYPASSRLS`, role, pw),
	); err != nil {
		t.Skipf("cannot CREATE ROLE (need a superuser DATABASE_URL): %v", err)
	}
	for _, g := range []string{
		fmt.Sprintf(`GRANT SELECT, INSERT, UPDATE, DELETE ON data_providers TO %s`, role),
		fmt.Sprintf(`GRANT SELECT ON advertiser_balances TO %s`, role),
		fmt.Sprintf(`GRANT SELECT ON accounts TO %s`, role),
	} {
		if _, err := super.ExecContext(ctx, g); err != nil {
			t.Fatalf("grant: %v", err)
		}
	}

	// Seed two throwaway tenants as the owner (exempt from RLS without FORCE).
	if _, err := super.ExecContext(ctx,
		`INSERT INTO accounts (id, name, email, type) VALUES
		   ($1,'RLS Probe A','rls-a@example.test','advertiser'),
		   ($2,'RLS Probe B','rls-b@example.test','advertiser')
		 ON CONFLICT (id) DO NOTHING`, accA, accB); err != nil {
		t.Fatalf("seed accounts: %v", err)
	}
	if _, err := super.ExecContext(ctx,
		`INSERT INTO data_providers (account_id, name) VALUES ($1,'probe-A'),($2,'probe-B')`,
		accA, accB); err != nil {
		t.Fatalf("seed data_providers: %v", err)
	}
	// advertiser_balances is a tenant table with a tenant_isolation policy
	// (migration 064) — used below to drive a REAL loader through QueryPlatform.
	if _, err := super.ExecContext(ctx,
		`INSERT INTO advertiser_balances (account_id) VALUES ($1),($2)`, accA, accB); err != nil {
		t.Fatalf("seed advertiser_balances: %v", err)
	}

	// Connect AS the limited role.
	u, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	u.User = url.UserPassword(role, pw)
	app, err := sql.Open("postgres", u.String())
	if err != nil {
		t.Fatalf("open as %s: %v", role, err)
	}
	// Registered AFTER the teardown so it runs BEFORE it — the role's
	// connections must be closed before teardown can DROP ROLE.
	t.Cleanup(func() { app.Close() })

	// namesWith runs SELECT name FROM data_providers in a tx after applying the
	// given session settings (key→value via set_config, tx-local).
	namesWith := func(t *testing.T, settings map[string]string) []string {
		t.Helper()
		tx, err := app.BeginTx(ctx, &sql.TxOptions{})
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback()
		for k, v := range settings {
			if _, err := tx.ExecContext(ctx, `SELECT set_config($1,$2,true)`, k, v); err != nil {
				t.Fatalf("set_config %s: %v", k, err)
			}
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT name FROM data_providers WHERE name LIKE 'probe-%' ORDER BY name`)
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, n)
		}
		return out
	}

	eq := func(t *testing.T, got, want []string) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	}

	t.Run("tenant_read_scoped_to_A", func(t *testing.T) {
		eq(t, namesWith(t, map[string]string{"app.current_account_id": accA}), []string{"probe-A"})
	})
	t.Run("platform_read_sees_all", func(t *testing.T) {
		eq(t, namesWith(t, map[string]string{"app.platform_read": "on"}), []string{"probe-A", "probe-B"})
	})
	t.Run("no_guc_sees_nothing_no_error", func(t *testing.T) {
		eq(t, namesWith(t, nil), nil)
	})
	t.Run("cross_tenant_write_blocked", func(t *testing.T) {
		tx, err := app.BeginTx(ctx, &sql.TxOptions{})
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id',$1,true)`, accA); err != nil {
			t.Fatalf("set_config: %v", err)
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM data_providers WHERE account_id = $1`, accB)
		if err != nil {
			t.Fatalf("delete: %v", err)
		}
		if n, _ := res.RowsAffected(); n != 0 {
			t.Errorf("tenant A deleted %d of tenant B's rows; want 0 (RLS should hide them)", n)
		}
		tx.Commit()
	})

	// End-to-end: a REAL warm-cache loader (BalanceLoader) driven through
	// QueryPlatform must see EVERY tenant's rows under the NOBYPASSRLS role —
	// this is the whole point of the platform-read hatch. A raw read with no GUC
	// would be filtered to nothing (proven by no_guc_sees_nothing_no_error above),
	// so seeing both probe balances proves QueryPlatform's SET LOCAL hatch works.
	t.Run("real_loader_sees_all_tenants", func(t *testing.T) {
		store := NewFromDB(app) // Store on the limited-role connection
		balances, err := (&BalanceLoader{Store: store}).LoadAll(ctx)
		if err != nil {
			t.Fatalf("BalanceLoader.LoadAll under NOBYPASSRLS role: %v", err)
		}
		got := map[string]bool{}
		for _, b := range balances {
			got[b.AccountID] = true
		}
		if !got[accA] || !got[accB] {
			t.Errorf("platform loader saw accA=%v accB=%v; want both (hatch failed → RLS filtered the loader)", got[accA], got[accB])
		}
	})
}
