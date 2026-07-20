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
	pgstore "github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

// TestDemoOnboardingRun_RealExpansion is the highest-value test: it runs the
// demo orchestrator against a REAL Postgres and the REAL pkg/profilebuilder, so
// it proves expansion actually happened — a one-email upload became a
// three-id (email+cookie+ifa) segment, with the household id deliberately left
// out. Skips when Postgres or the required tables are unavailable.
func TestDemoOnboardingRun_RealExpansion(t *testing.T) {
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

	o := &demoOrchestrator{
		db:       db,
		aud:      audiencepg.New(db),
		resolver: pgstore.NewFromDB(db),
		bus:      nil, // Bus is optional; the demo expands without it.
		log:      quietLog(),
	}
	t.Cleanup(func() {
		acct := demoAccountID()
		db.Exec(`DELETE FROM audience_segment_members WHERE account_id = $1::uuid`, acct)
		db.Exec(`DELETE FROM audience_segments WHERE account_id = $1::uuid`, acct)
		for _, id := range demoAllIDs() {
			db.Exec(`DELETE FROM identity_graph WHERE user_id = $1 OR linked_id = $1`, id)
			db.Exec(`DELETE FROM identity_clusters WHERE member_id = $1`, id)
		}
	})

	resp, err := o.run(ctx)
	if err != nil {
		t.Fatalf("demo run: %v", err)
	}
	if !resp.Ran || len(resp.Steps) != 5 {
		t.Fatalf("ran=%v steps=%d, want ran + 5 steps", resp.Ran, len(resp.Steps))
	}

	// Steps must be numbered 1..5 in order.
	for i, s := range resp.Steps {
		if s.N != i+1 {
			t.Fatalf("step %d has N=%d, want %d", i, s.N, i+1)
		}
	}

	// Pull the BEFORE (step 3) and AFTER (step 5) membership sets.
	before := stepMembers(t, resp.Steps[2]) // step 3
	after := stepMembers(t, resp.Steps[4])  // step 5

	// BEFORE = exactly the email.
	if len(before) != 1 || before[0] != demoEmailID {
		t.Fatalf("BEFORE members = %v, want [%s]", before, demoEmailID)
	}

	// AFTER superset-contains BEFORE.
	afterSet := map[string]bool{}
	for _, m := range after {
		afterSet[m] = true
	}
	for _, b := range before {
		if !afterSet[b] {
			t.Fatalf("AFTER %v does not superset-contain BEFORE %v", after, before)
		}
	}

	// AFTER = {email, cookie, ifa} exactly, and the household is EXCLUDED.
	want := map[string]bool{demoEmailID: true, demoCookieID: true, demoIfaID: true}
	if len(after) != len(want) {
		t.Fatalf("AFTER members = %v, want %v", after, mapKeys(want))
	}
	for _, m := range after {
		if !want[m] {
			t.Fatalf("unexpected AFTER member %q (members=%v)", m, after)
		}
	}
	if afterSet[demoHouseholdID] {
		t.Fatalf("household id %q leaked into the expanded segment: %v", demoHouseholdID, after)
	}

	// Expansion must have actually added the two device ids.
	added := demoDiff(before, after)
	if len(added) != 2 {
		t.Fatalf("expansion added %v, want [cookie, ifa]", added)
	}
}

// stepMembers extracts segment_members from a step's data map.
func stepMembers(t *testing.T, s demoStep) []string {
	t.Helper()
	m, ok := s.Data.(map[string]interface{})
	if !ok {
		t.Fatalf("step %d data is not a map: %T", s.N, s.Data)
	}
	raw, ok := m["segment_members"].([]string)
	if !ok {
		t.Fatalf("step %d has no []string segment_members (got %T)", s.N, m["segment_members"])
	}
	return raw
}

func mapKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
