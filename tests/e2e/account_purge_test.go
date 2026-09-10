//go:build e2e

// 90-day account purge (the last account-closure deferral): once a closed
// account's retention window elapses, account-closeout destructively removes its
// data across Postgres (every account-keyed table) + ClickHouse (analytics) and
// marks the closure 'purged'. The accounts row + closure record survive as a
// tombstone. This drives it exactly as the daily cron does.
package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestAccountPurgeAfterRetention(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "purge")
	adv := w.AdvAcc

	// Real data: a campaign (Postgres) + an impression (ClickHouse) under the
	// account, so the purge has something to delete on both sides.
	auc := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "purge-user-1")
	win := h.ExtractWinner(t, auc)
	if win.NoBid {
		t.Fatal("expected a winning bid")
	}
	h.FireImpression(t, auc.TraceID, win.CampaignID, win.CreativeID,
		auc.PlacementID, auc.PublisherID, adv.ID, "USD", win.Price)

	// Baselines (must be non-zero so the purge assertions aren't vacuous).
	var campaignsBefore int
	if err := h.DB.QueryRow(`SELECT count(*) FROM line_items WHERE account_id = $1::uuid`, adv.ID).Scan(&campaignsBefore); err != nil {
		t.Fatalf("campaigns before: %v", err)
	}
	if campaignsBefore == 0 {
		t.Fatalf("no campaigns for the account before purge — test setup broken")
	}

	// A financial record (invoices) that the purge MUST retain — settlement/tax
	// records survive the destructive purge (only the account's private operational
	// data is wiped; invoices/payouts/adjustments are on the keep-side allowlist).
	if _, err := h.DB.Exec(
		`INSERT INTO invoices (account_id, total, currency, period_start, period_end, due_date)
		 VALUES ($1::uuid, 12.34, 'USD', now() - interval '30 days', now(), now() + interval '30 days')`,
		adv.ID); err != nil {
		t.Fatalf("seed retained invoice: %v", err)
	}
	harness.WaitFor(t, 30*time.Second, "impression in ClickHouse", func() bool {
		return chImpressionsForAccount(t, adv.ID) >= 1
	})

	chPort := startCHPortForward(t)
	runCloseout := func() {
		t.Helper()
		cmd := exec.Command("go", "run", "./cmd/account-closeout")
		cmd.Dir = "../.."
		cmd.Env = append(os.Environ(),
			"CLICKHOUSE_ADDR=127.0.0.1:"+chPort,
			"DATABASE_URL=postgres://adtech_app:adtech-app-local@localhost:5432/adtech?sslmode=disable")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("account-closeout failed: %v\n%s", err, out)
		}
	}

	// Close the account (grace → closed) via a due closure.
	client := h.OwnerClient(t, adv.ID)
	if st, _ := h.AccountClose(t, client); st != 200 {
		t.Fatalf("close initiate status %d", st)
	}
	if _, err := h.DB.Exec(
		`UPDATE account_closure_requests SET grace_ends_at = now() - interval '1 hour'
		 WHERE account_id = $1::uuid AND status = 'grace'`, adv.ID); err != nil {
		t.Fatalf("backdate grace: %v", err)
	}
	runCloseout()

	// Backdate closed_at past the retention window so the purge is due, then run
	// close-out again — this time it purges.
	if _, err := h.DB.Exec(
		`UPDATE account_closure_requests SET closed_at = now() - interval '91 days'
		 WHERE account_id = $1::uuid AND status = 'closed'`, adv.ID); err != nil {
		t.Fatalf("backdate closed_at: %v", err)
	}
	runCloseout()

	// Postgres data is gone.
	var campaignsAfter int
	if err := h.DB.QueryRow(`SELECT count(*) FROM line_items WHERE account_id = $1::uuid`, adv.ID).Scan(&campaignsAfter); err != nil {
		t.Fatalf("campaigns after: %v", err)
	}
	if campaignsAfter != 0 {
		t.Errorf("line_items after purge = %d, want 0 (Postgres data not purged)", campaignsAfter)
	}

	// ClickHouse data is gone.
	if n := chImpressionsForAccount(t, adv.ID); n != 0 {
		t.Errorf("ClickHouse impressions after purge = %d, want 0 (analytics not purged)", n)
	}

	// The financial record SURVIVES — the purge must not destroy settlement/tax
	// records (the allowlist excludes invoices/payouts/adjustments).
	var invoicesAfter int
	if err := h.DB.QueryRow(`SELECT count(*) FROM invoices WHERE account_id = $1::uuid`, adv.ID).Scan(&invoicesAfter); err != nil {
		t.Fatalf("invoices after: %v", err)
	}
	if invoicesAfter != 1 {
		t.Errorf("invoices after purge = %d, want 1 (financial records must be retained)", invoicesAfter)
	}

	// Closure flipped to the terminal 'purged' state with purged_at set.
	var status string
	var purgedAt *time.Time
	if err := h.DB.QueryRow(
		`SELECT status, purged_at FROM account_closure_requests WHERE account_id = $1::uuid ORDER BY requested_at DESC LIMIT 1`,
		adv.ID).Scan(&status, &purgedAt); err != nil {
		t.Fatalf("closure row: %v", err)
	}
	if status != "purged" {
		t.Errorf("closure status = %q, want purged", status)
	}
	if purgedAt == nil {
		t.Errorf("purged_at not set")
	}

	// The accounts row survives as a tombstone (the closure FK depends on it).
	var accounts int
	if err := h.DB.QueryRow(`SELECT count(*) FROM accounts WHERE id = $1::uuid`, adv.ID).Scan(&accounts); err != nil {
		t.Fatalf("accounts tombstone: %v", err)
	}
	if accounts != 1 {
		t.Errorf("accounts row count = %d, want 1 (the tombstone must survive)", accounts)
	}

	// Idempotent: the closure is now 'purged', not 'closed', so a re-run selects
	// nothing and does not error.
	runCloseout()
}

// chImpressionsForAccount counts an account's ClickHouse impressions via kubectl
// exec (CH native isn't host-reachable except through the per-test port-forward).
func chImpressionsForAccount(t *testing.T, accountID string) int {
	t.Helper()
	q := fmt.Sprintf("SELECT count() FROM adtech.impressions WHERE account_id='%s'", accountID)
	out, err := exec.Command("kubectl", "-n", "adtech", "exec", "-i", "clickhouse-0", "--",
		"clickhouse-client", "-q", q).Output()
	if err != nil {
		return -1
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}
