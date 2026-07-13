//go:build integration

package reportjobs

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// TestPostgresJobStore runs the shared JobStore suite against a real Postgres
// (never mocked, per repo convention). It uses DATABASE_URL or the local stack
// default, and skips when Postgres is unreachable or migration 039 has not
// been applied yet.
func TestPostgresJobStore(t *testing.T) {
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
	var exists bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'report_jobs')`,
	).Scan(&exists); err != nil || !exists {
		t.Skipf("report_jobs table missing (migration 039 not applied): %v", err)
	}

	// Two throwaway accounts; ON DELETE CASCADE cleans up every job row.
	suffix := time.Now().UnixNano()
	accA := createAccount(t, db, fmt.Sprintf("rjtest-a-%d", suffix))
	accB := createAccount(t, db, fmt.Sprintf("rjtest-b-%d", suffix))
	t.Cleanup(func() {
		db.Exec(`DELETE FROM accounts WHERE id IN ($1::uuid, $2::uuid)`, accA, accB)
	})

	savedReportIDHook = func(t *testing.T, accountID string) string {
		var id string
		err := db.QueryRow(`INSERT INTO saved_reports (account_id, name, query_config)
		                    VALUES ($1::uuid, 'suite schedule', '{}') RETURNING id::text`,
			accountID).Scan(&id)
		if err != nil {
			t.Fatalf("insert saved_report: %v", err)
		}
		return id
	}
	defer func() { savedReportIDHook = nil }()

	store := NewPostgresJobStore(db)
	runJobStoreSuite(t, store, accA, accB)

	t.Run("concurrent_claims_take_distinct_jobs", func(t *testing.T) {
		const n = 8
		for i := 0; i < n; i++ {
			if _, err := store.Enqueue(ctx, suiteJob(accA, fmt.Sprintf("concurrent-%d", i))); err != nil {
				t.Fatalf("enqueue: %v", err)
			}
		}
		var mu sync.Mutex
		seen := map[string]int{}
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				j, err := store.ClaimOne(ctx)
				if err != nil || j == nil {
					t.Errorf("concurrent claim: %v %v", j, err)
					return
				}
				mu.Lock()
				seen[j.ID]++
				mu.Unlock()
				store.MarkDone(ctx, j.ID, Artifact{})
			}()
		}
		wg.Wait()
		if len(seen) != n {
			t.Errorf("claimed %d distinct jobs, want %d", len(seen), n)
		}
		for id, c := range seen {
			if c > 1 {
				t.Errorf("job %s claimed %d times", id, c)
			}
		}
	})
}

func createAccount(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var id string
	if err := db.QueryRow(`INSERT INTO accounts (name, email, type)
	                       VALUES ($1, $1 || '@test.local', 'advertiser') RETURNING id::text`,
		name).Scan(&id); err != nil {
		t.Fatalf("create account %s: %v", name, err)
	}
	return id
}
