//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"sort"
	"testing"

	_ "github.com/lib/pq"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// TestAudienceWithPlatformRead proves the audience store's cross-tenant reads
// (SegmentsForUser — the SSP's user.ext.segments stamp on the bid-request hot
// path) work under a real NOBYPASSRLS role via the withPlatformRead hatch
// (security #77). A public segment belongs to a specific account, but the SSP
// fan-out must see EVERY tenant's public segments for a user; without the hatch
// RLS would blank the join and the SSP would stamp nothing.
//
// Skips cleanly when Postgres is unreachable, the platform-read hatch migration
// isn't applied, or the connecting role can't CREATE ROLE. Everything it
// creates is cleaned up (LIFO ordering: role conn closed before DROP ROLE).
func TestAudienceWithPlatformRead(t *testing.T) {
	baseURL := os.Getenv("DATABASE_URL")
	if baseURL == "" {
		baseURL = routes.DefaultPostgresURL
	}
	ctx := context.Background()

	super, err := sql.Open("postgres", baseURL)
	if err != nil {
		t.Skipf("postgres open: %v", err)
	}
	t.Cleanup(func() { super.Close() })
	if err := super.PingContext(ctx); err != nil {
		t.Skipf("postgres unreachable (%v) — start the local stack or set DATABASE_URL", err)
	}

	// Skip unless the audience_segments policy carries the platform-read hatch.
	var hatched int
	if err := super.QueryRowContext(ctx,
		`SELECT count(*) FROM pg_policies
		 WHERE tablename = 'audience_segments' AND qual LIKE '%app.platform_read%'`,
	).Scan(&hatched); err != nil || hatched == 0 {
		t.Skipf("audience_segments platform-read hatch not applied: hatched=%d err=%v", hatched, err)
	}

	const (
		role = "aud_rls_probe"
		pw   = "probe"
		accA = "aaaaaaaa-0000-4000-8000-0000000000ac"
		accB = "bbbbbbbb-0000-4000-8000-0000000000bc"
		segA = "11111111-0000-4000-8000-00000000a111"
		segB = "22222222-0000-4000-8000-00000000b222"
		user = "aud-probe-user"
	)

	cleanup := func() {
		super.ExecContext(ctx, `DELETE FROM audience_segment_members WHERE user_id = $1`, user)
		super.ExecContext(ctx, `DELETE FROM audience_segments WHERE id IN ($1,$2)`, segA, segB)
		super.ExecContext(ctx, `DELETE FROM accounts WHERE id IN ($1,$2)`, accA, accB)
		super.ExecContext(ctx, `DO $$ BEGIN
			IF EXISTS (SELECT FROM pg_roles WHERE rolname = '`+role+`') THEN
				EXECUTE 'DROP OWNED BY `+role+`'; EXECUTE 'DROP ROLE `+role+`';
			END IF; END $$;`)
	}
	cleanup()
	t.Cleanup(cleanup)

	if _, err := super.ExecContext(ctx,
		fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOBYPASSRLS`, role, pw),
	); err != nil {
		t.Skipf("cannot CREATE ROLE (need a superuser DATABASE_URL): %v", err)
	}
	for _, g := range []string{
		fmt.Sprintf(`GRANT SELECT ON audience_segments TO %s`, role),
		fmt.Sprintf(`GRANT SELECT ON audience_segment_members TO %s`, role),
	} {
		if _, err := super.ExecContext(ctx, g); err != nil {
			t.Fatalf("grant: %v", err)
		}
	}

	// Seed two tenants, each with a PUBLIC segment the same user belongs to.
	if _, err := super.ExecContext(ctx,
		`INSERT INTO accounts (id, name, email, type) VALUES
		   ($1,'Aud Probe A','aud-a@example.test','advertiser'),
		   ($2,'Aud Probe B','aud-b@example.test','advertiser')
		 ON CONFLICT (id) DO NOTHING`, accA, accB); err != nil {
		t.Fatalf("seed accounts: %v", err)
	}
	if _, err := super.ExecContext(ctx,
		`INSERT INTO audience_segments (id, account_id, name, type, visibility, status) VALUES
		   ($1,$2,'aud-seg-A','first_party','public','active'),
		   ($3,$4,'aud-seg-B','first_party','public','active')`,
		segA, accA, segB, accB); err != nil {
		t.Fatalf("seed segments: %v", err)
	}
	if _, err := super.ExecContext(ctx,
		`INSERT INTO audience_segment_members (segment_id, user_id, account_id) VALUES
		   ($1,$3,$2),($4,$3,$5)`,
		segA, accA, user, segB, accB); err != nil {
		t.Fatalf("seed members: %v", err)
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
	t.Cleanup(func() { app.Close() })

	// Baseline: a raw cross-tenant join with NO GUC set sees nothing under RLS —
	// this is exactly what SegmentsForUser would return if it forgot the hatch.
	t.Run("no_hatch_sees_nothing", func(t *testing.T) {
		var n int
		if err := app.QueryRowContext(ctx,
			`SELECT count(*) FROM audience_segment_members m
			   JOIN audience_segments s ON s.id = m.segment_id
			 WHERE m.user_id = $1 AND s.visibility = 'public'`, user).Scan(&n); err != nil {
			t.Fatalf("baseline count: %v", err)
		}
		if n != 0 {
			t.Errorf("no-GUC cross-tenant join saw %d rows; want 0 (RLS blanks it)", n)
		}
	})

	// SegmentsForUser goes through withPlatformRead → sees BOTH tenants' public
	// segments for the user, under the NOBYPASSRLS role.
	t.Run("SegmentsForUser_sees_all_tenants", func(t *testing.T) {
		got, err := New(app).SegmentsForUser(ctx, user)
		if err != nil {
			t.Fatalf("SegmentsForUser under NOBYPASSRLS role: %v", err)
		}
		sort.Strings(got)
		want := []string{segA, segB}
		sort.Strings(want)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("SegmentsForUser = %v, want %v (hatch must span both tenants)", got, want)
		}
	})
}
