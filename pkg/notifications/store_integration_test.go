//go:build integration

package notifications

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// TestPostgresStore runs the notification store against a real Postgres (never
// mocked, per repo convention). Uses DATABASE_URL or the local stack default;
// skips when Postgres is unreachable or migration 049 has not been applied.
func TestPostgresStore(t *testing.T) {
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
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'notifications')`,
	).Scan(&exists); err != nil || !exists {
		t.Skipf("notifications table missing (migration 049 not applied): %v", err)
	}

	suffix := time.Now().UnixNano()
	accA := createAccount(t, db, fmt.Sprintf("notiftest-a-%d", suffix))
	accB := createAccount(t, db, fmt.Sprintf("notiftest-b-%d", suffix))
	t.Cleanup(func() {
		db.Exec(`DELETE FROM accounts WHERE id IN ($1::uuid, $2::uuid)`, accA, accB)
	})

	store := NewPostgresStore(db)

	// Two rows for A (one carrying a ref), one for B.
	mustInsert(t, store, Notification{AccountID: accA, Kind: KindBudgetDepleted, Title: "Budget", Body: "spent", RefID: "camp-1"})
	mustInsert(t, store, Notification{AccountID: accA, Kind: KindCampaignState, Title: "Paused"})
	mustInsert(t, store, Notification{AccountID: accB, Kind: KindBalanceDepleted, Title: "Balance"})

	// List is tenant-scoped: A sees only its two.
	listA, err := store.ListForAccount(ctx, accA, 50)
	if err != nil {
		t.Fatalf("list A: %v", err)
	}
	if len(listA) != 2 {
		t.Fatalf("A list = %d rows, want 2 (tenant isolation): %+v", len(listA), listA)
	}
	// Newest first: the campaign-state row was inserted last.
	if listA[0].Kind != KindCampaignState {
		t.Errorf("list not newest-first: %+v", listA)
	}
	// Ref survived the round-trip.
	var sawRef bool
	for _, n := range listA {
		if n.RefID == "camp-1" {
			sawRef = true
		}
	}
	if !sawRef {
		t.Error("ref_id not persisted/returned")
	}

	// Unread count is tenant-scoped.
	if n, _ := store.UnreadCount(ctx, accA); n != 2 {
		t.Errorf("A unread = %d, want 2", n)
	}
	if n, _ := store.UnreadCount(ctx, accB); n != 1 {
		t.Errorf("B unread = %d, want 1", n)
	}

	// Cross-tenant mark-read is a no-op: A cannot mark B's row.
	if err := store.MarkRead(ctx, accA, listBFirstID(t, store, accB)); err != sql.ErrNoRows {
		t.Errorf("cross-tenant MarkRead err = %v, want ErrNoRows", err)
	}
	if n, _ := store.UnreadCount(ctx, accB); n != 1 {
		t.Errorf("B unread after cross-tenant mark = %d, want still 1", n)
	}

	// MarkRead one of A's own, then MarkAllRead clears the rest.
	if err := store.MarkRead(ctx, accA, listA[0].ID); err != nil {
		t.Fatalf("MarkRead own: %v", err)
	}
	if n, _ := store.UnreadCount(ctx, accA); n != 1 {
		t.Errorf("A unread after one mark = %d, want 1", n)
	}
	if err := store.MarkAllRead(ctx, accA); err != nil {
		t.Fatalf("MarkAllRead: %v", err)
	}
	if n, _ := store.UnreadCount(ctx, accA); n != 0 {
		t.Errorf("A unread after mark-all = %d, want 0", n)
	}
}

func mustInsert(t *testing.T, store *PostgresStore, n Notification) {
	t.Helper()
	if err := store.Insert(context.Background(), n); err != nil {
		t.Fatalf("insert: %v", err)
	}
}

func listBFirstID(t *testing.T, store *PostgresStore, accountID string) string {
	t.Helper()
	list, err := store.ListForAccount(context.Background(), accountID, 1)
	if err != nil || len(list) == 0 {
		t.Fatalf("list %s: %v", accountID, err)
	}
	return list[0].ID
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
