//go:build integration

package main

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/lib/pq"

	audiencepg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/datalake"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/fs"
	pgstore "github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

// TestDemoRetargetingRun_RealMatch is the highest-value test: it runs the demo
// orchestrator against a REAL Postgres, a REAL lake ObjectStore (over a temp
// filesystem — the same pure-Go Arrow-Parquet + Delta code path Minio uses),
// and the REAL pkg/profilebuilder. It proves the whole retargeting flow: a
// seeded site_visit behaviour_signals row lands in the lake, the behavioural
// rule matches it, and the demo user is enrolled AND cluster-expanded to
// {email, cookie, ifa} — with the household id deliberately left out. Skips when
// Postgres or the required tables are unavailable.
func TestDemoRetargetingRun_RealMatch(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = routes.DefaultPostgresURL
	}
	db, err := sql.Open("postgres", url)
	if err != nil {
		t.Skipf("postgres open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		t.Skipf("postgres unreachable (%v) — start the local stack or set DATABASE_URL", err)
	}
	for _, tbl := range []string{"identity_graph", "identity_clusters", "audience_segments", "audience_segment_members", "accounts"} {
		var exists bool
		if err := db.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`, tbl).Scan(&exists); err != nil || !exists {
			t.Skipf("table %q missing (migrations not applied): %v", tbl, err)
		}
	}

	// A REAL lake over a per-test temp filesystem: datalake.NewObjectStore is
	// the exact production reader/writer (pure Go, no duckdb tag), just pointed
	// at fs instead of Minio. Nothing is mocked in the flow.
	obj, err := fs.New(t.TempDir())
	if err != nil {
		t.Fatalf("fs object store: %v", err)
	}
	lake := datalake.NewObjectStore(obj, "adtech-datalake", quietLog())

	backend := &httpLakeRetargetingBackend{
		// No pixel fire in the test — trackerURL empty makes FirePixel report
		// unreachable (non-fatal); the deterministic builder input is the lake
		// write + the real profile-builder run.
		client:     nil,
		trackerURL: "",
		db:         db,
		lake:       lake,
		bus:        nil, // Bus optional; the demo expands without it.
		log:        quietLog(),
	}
	o := &rtDemoOrchestrator{
		db:       db,
		aud:      audiencepg.New(db),
		resolver: pgstore.NewFromDB(db),
		backend:  backend,
		log:      quietLog(),
	}
	t.Cleanup(func() {
		acct := rtAccountID()
		db.Exec(`DELETE FROM audience_segment_members WHERE account_id = $1::uuid`, acct)
		db.Exec(`DELETE FROM audience_segments WHERE account_id = $1::uuid`, acct)
		for _, id := range rtAllIDs() {
			db.Exec(`DELETE FROM identity_graph WHERE user_id = $1 OR linked_id = $1`, id)
			db.Exec(`DELETE FROM identity_clusters WHERE member_id = $1`, id)
		}
		_ = lake.TruncateTable(context.Background(), behaviourSignalsLakeTable)
	})

	resp, err := o.run(ctx)
	if err != nil {
		t.Fatalf("demo run: %v", err)
	}
	if !resp.Ran || len(resp.Steps) != 5 {
		t.Fatalf("ran=%v steps=%d, want ran + 5 steps", resp.Ran, len(resp.Steps))
	}
	for i, s := range resp.Steps {
		if s.N != i+1 {
			t.Fatalf("step %d has N=%d, want %d", i, s.N, i+1)
		}
	}

	before := stepMembers(t, resp.Steps[2]) // step 3 BEFORE
	after := stepMembers(t, resp.Steps[4])  // step 5 AFTER

	// BEFORE = empty: the visit landed a behaviour row but the rule hadn't run.
	if len(before) != 0 {
		t.Fatalf("BEFORE members = %v, want [] (rule not yet run)", before)
	}

	// AFTER = {email, cookie, ifa} exactly — the rule matched the site_visit
	// row (enrolled the email) and cluster-expanded across the two devices; the
	// household id is EXCLUDED.
	want := map[string]bool{rtEmailID: true, rtCookieID: true, rtIfaID: true}
	if len(after) != len(want) {
		t.Fatalf("AFTER members = %v, want %v", after, mapKeys(want))
	}
	for _, m := range after {
		if !want[m] {
			t.Fatalf("unexpected AFTER member %q (members=%v)", m, after)
		}
		if m == rtHouseholdID {
			t.Fatalf("household id leaked into the retargeting segment: %v", after)
		}
	}

	// A visit really enrolled someone: the rule added the email + expanded.
	if added := demoDiff(before, after); len(added) != 3 {
		t.Fatalf("rule enrolled+expanded %v ids, want 3 (email, cookie, ifa)", added)
	}
}
