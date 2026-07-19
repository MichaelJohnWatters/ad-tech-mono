//go:build integration

package invoicing

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

// TestGenerator runs the invoice Generator against a real Postgres (never
// mocked, per repo convention). It seeds committed spend for two campaigns of
// one advertiser plus a second advertiser (to prove tenant scoping in
// GenerateForAllAccounts), then asserts the micros→dollars total, one line per
// campaign, and that a re-run is idempotent (no duplicate invoice).
func TestGenerator(t *testing.T) {
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
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'invoice_line_items')`,
	).Scan(&exists); err != nil || !exists {
		t.Skipf("invoice_line_items table missing (migration 051 not applied): %v", err)
	}

	suffix := time.Now().UnixNano()
	accA := createAccount(t, db, fmt.Sprintf("invtest-a-%d", suffix))
	accB := createAccount(t, db, fmt.Sprintf("invtest-b-%d", suffix))
	t.Cleanup(func() {
		// invoices/line-items cascade via account FK + invoice FK; committed
		// spend is FK-free so delete it explicitly by the campaign ids we made.
		db.Exec(`DELETE FROM invoices WHERE account_id IN ($1::uuid, $2::uuid)`, accA, accB)
		db.Exec(`DELETE FROM accounts WHERE id IN ($1::uuid, $2::uuid)`, accA, accB)
	})

	// Period: a fixed month well clear of any real data.
	periodStart := time.Date(2099, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2099, 4, 1, 0, 0, 0, 0, time.UTC)
	dayIn := "2099-03-15"
	dayOut := "2099-04-15" // outside the period — must be excluded

	// Advertiser A: two campaigns. c1 = 1,500,000 micros ($1.50) across two
	// days; c2 = 500,000 micros ($0.50). Total = $2.00.
	c1 := createLineItem(t, db, accA, "Campaign One", "cpm")
	c2 := createLineItem(t, db, accA, "Campaign Two", "cpc")
	insertCommittedSpend(t, db, dayIn, c1, 1_000_000)
	insertCommittedSpend(t, db, "2099-03-16", c1, 500_000)
	insertCommittedSpend(t, db, dayIn, c2, 500_000)
	// Spend outside the period must not be billed.
	insertCommittedSpend(t, db, dayOut, c1, 9_000_000)

	// Advertiser B: one campaign, $3.00.
	cB := createLineItem(t, db, accB, "B Campaign", "cpm")
	insertCommittedSpend(t, db, dayIn, cB, 3_000_000)

	t.Cleanup(func() {
		db.Exec(`DELETE FROM campaign_committed_spend WHERE campaign_id IN ($1,$2,$3)`, c1, c2, cB)
	})

	gen := New(db)

	// GenerateForAccount(A): total $2.00, two lines.
	invID, err := gen.GenerateForAccount(ctx, accA, periodStart, periodEnd)
	if err != nil {
		t.Fatalf("generate A: %v", err)
	}
	if invID == "" {
		t.Fatal("expected an invoice id for account with spend")
	}
	assertInvoiceTotal(t, db, invID, 2.00)
	if n := lineCount(t, db, invID); n != 2 {
		t.Fatalf("line count = %d, want 2", n)
	}

	// Idempotent re-run: same invoice id, still one invoice, still two lines.
	invID2, err := gen.GenerateForAccount(ctx, accA, periodStart, periodEnd)
	if err != nil {
		t.Fatalf("re-run A: %v", err)
	}
	if invID2 != invID {
		t.Fatalf("re-run produced a new invoice id %q (want %q) — not idempotent", invID2, invID)
	}
	if n := invoiceCount(t, db, accA); n != 1 {
		t.Fatalf("invoice count after re-run = %d, want 1", n)
	}
	if n := lineCount(t, db, invID); n != 2 {
		t.Fatalf("line count after re-run = %d, want 2", n)
	}

	// Account with no spend → no invoice, no error.
	accC := createAccount(t, db, fmt.Sprintf("invtest-c-%d", suffix))
	t.Cleanup(func() { db.Exec(`DELETE FROM accounts WHERE id = $1::uuid`, accC) })
	emptyID, err := gen.GenerateForAccount(ctx, accC, periodStart, periodEnd)
	if err != nil {
		t.Fatalf("generate empty: %v", err)
	}
	if emptyID != "" {
		t.Fatalf("expected no invoice for spend-free account, got %q", emptyID)
	}

	// GenerateForAllAccounts: hits A (already has its invoice) + B.
	ids, err := gen.GenerateForAllAccounts(ctx, periodStart, periodEnd)
	if err != nil {
		t.Fatalf("generate all: %v", err)
	}
	if len(ids) < 2 {
		t.Fatalf("generate all wrote %d invoices, want >= 2 (A and B)", len(ids))
	}
	if n := invoiceCount(t, db, accB); n != 1 {
		t.Fatalf("account B invoice count = %d, want 1", n)
	}
	assertAccountInvoiceTotal(t, db, accB, 3.00)
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

func createLineItem(t *testing.T, db *sql.DB, accountID, name, bidStrategy string) string {
	t.Helper()
	var ioID string
	if err := db.QueryRow(`INSERT INTO insertion_orders (account_id, name, budget, start_date, end_date)
	                       VALUES ($1::uuid, $2, 1000, '2099-03-01', '2099-04-01') RETURNING id::text`,
		accountID, name+" IO").Scan(&ioID); err != nil {
		t.Fatalf("create IO: %v", err)
	}
	var liID string
	if err := db.QueryRow(`INSERT INTO line_items (account_id, insertion_order_id, name, base_bid, bid_strategy)
	                       VALUES ($1::uuid, $2::uuid, $3, 1.0, $4) RETURNING id::text`,
		accountID, ioID, name, bidStrategy).Scan(&liID); err != nil {
		t.Fatalf("create line item: %v", err)
	}
	return liID
}

func insertCommittedSpend(t *testing.T, db *sql.DB, day, campaignID string, settledMicros int64) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO campaign_committed_spend (day, campaign_id, settled_micros, reserved_micros, updated_at)
	                      VALUES ($1, $2, $3, 0, now())
	                      ON CONFLICT (day, campaign_id) DO UPDATE SET settled_micros = EXCLUDED.settled_micros`,
		day, campaignID, settledMicros); err != nil {
		t.Fatalf("insert committed spend: %v", err)
	}
}

func assertInvoiceTotal(t *testing.T, db *sql.DB, invoiceID string, want float64) {
	t.Helper()
	var total float64
	if err := db.QueryRow(`SELECT total FROM invoices WHERE id = $1::uuid`, invoiceID).Scan(&total); err != nil {
		t.Fatalf("read total: %v", err)
	}
	if total != want {
		t.Fatalf("invoice total = %v, want %v", total, want)
	}
}

func assertAccountInvoiceTotal(t *testing.T, db *sql.DB, accountID string, want float64) {
	t.Helper()
	var total float64
	if err := db.QueryRow(`SELECT total FROM invoices WHERE account_id = $1::uuid`, accountID).Scan(&total); err != nil {
		t.Fatalf("read account total: %v", err)
	}
	if total != want {
		t.Fatalf("account invoice total = %v, want %v", total, want)
	}
}

func lineCount(t *testing.T, db *sql.DB, invoiceID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM invoice_line_items WHERE invoice_id = $1::uuid`, invoiceID).Scan(&n); err != nil {
		t.Fatalf("count lines: %v", err)
	}
	return n
}

func invoiceCount(t *testing.T, db *sql.DB, accountID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM invoices WHERE account_id = $1::uuid`, accountID).Scan(&n); err != nil {
		t.Fatalf("count invoices: %v", err)
	}
	return n
}
