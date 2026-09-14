//go:build e2e

// Account Closure and Data Export (PLAN Phase 11, item 105) — slice 3: final
// settlement + close-out. When a closure's 30-day grace period elapses, the
// account-closeout job generates a final invoice (from un-billed settled spend),
// marks the account closed and closes the request. The 90-day data purge is
// deferred (closed_at is left set for the future purge job).
package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestAccountCloseoutFinalizesAndInvoices(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "closeout")
	adv := w.AdvAcc

	// Real settled spend so the final invoice has something to bill.
	auc := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "closeout-user-1")
	win := h.ExtractWinner(t, auc)
	if win.NoBid {
		t.Fatal("expected a winning bid for the close-out world")
	}
	h.FireImpression(t, auc.TraceID, win.CampaignID, win.CreativeID,
		auc.PlacementID, auc.PublisherID, adv.ID, "USD", win.Price)
	harness.WaitFor(t, 30*time.Second, "settled spend to land", func() bool {
		var micros int64
		err := h.DB.QueryRow(
			`SELECT COALESCE(SUM(settled_micros),0) FROM campaign_committed_spend WHERE campaign_id = $1`,
			win.CampaignID).Scan(&micros)
		return err == nil && micros > 0
	})

	// Owner initiates closure (grace).
	client := h.OwnerClient(t, adv.ID)
	if st, _ := h.AccountClose(t, client); st != 200 {
		t.Fatalf("close initiate status %d", st)
	}

	// Backdate the grace window so the close-out job treats it as due (no 30-day
	// wait). This is the only shortcut — the job logic is exercised for real.
	if _, err := h.DB.Exec(
		`UPDATE account_closure_requests SET grace_ends_at = now() - interval '1 hour'
		 WHERE account_id = $1::uuid AND status = 'grace'`, adv.ID); err != nil {
		t.Fatalf("backdate grace: %v", err)
	}

	// Run the close-out job exactly as the daily cron / ops would.
	cmd := exec.Command("go", "run", "./cmd/account-closeout")
	cmd.Dir = "../.."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("account-closeout failed: %v\n%s", err, out)
	}

	// Account is closed; closure is closed with closed_at set.
	if got := accountStatus(t, h, adv.ID); got != "closed" {
		t.Errorf("account status = %q, want closed", got)
	}
	var closureStatus string
	var closedAt *time.Time
	if err := h.DB.QueryRow(
		`SELECT status, closed_at FROM account_closure_requests WHERE account_id = $1::uuid ORDER BY requested_at DESC LIMIT 1`,
		adv.ID).Scan(&closureStatus, &closedAt); err != nil {
		t.Fatalf("closure row: %v", err)
	}
	if closureStatus != "closed" {
		t.Errorf("closure status = %q, want closed", closureStatus)
	}
	if closedAt == nil {
		t.Errorf("closed_at not set — the purge job scans on it")
	}

	// A final invoice was generated from the un-billed settled spend.
	var invoices int
	var total float64
	if err := h.DB.QueryRow(
		`SELECT count(*), COALESCE(SUM(total),0) FROM invoices WHERE account_id = $1`, adv.ID).Scan(&invoices, &total); err != nil {
		t.Fatalf("invoices query: %v", err)
	}
	if invoices < 1 || total <= 0 {
		t.Errorf("final invoice: count=%d total=%v, want >=1 invoice with positive total", invoices, total)
	}

	// Idempotent: a second run does not re-close or double-invoice (the closure is
	// no longer 'grace', so it's not selected again).
	cmd2 := exec.Command("go", "run", "./cmd/account-closeout")
	cmd2.Dir = "../.."
	if out, err := cmd2.CombinedOutput(); err != nil {
		t.Fatalf("account-closeout re-run failed: %v\n%s", err, out)
	}
	var invoices2 int
	if err := h.DB.QueryRow(`SELECT count(*) FROM invoices WHERE account_id = $1`, adv.ID).Scan(&invoices2); err != nil {
		t.Fatalf("invoices recount: %v", err)
	}
	if invoices2 != invoices {
		t.Errorf("re-run changed invoice count %d → %d (not idempotent)", invoices, invoices2)
	}
}

// TestAccountCloseoutFinalPublisherPayout — the publisher mirror: when a
// publisher account's grace elapses, the close-out generates a FINAL PAYOUT for
// its un-paid revenue (via the payout generator, ClickHouse gross × contract),
// then marks the account closed. Proves the deferred hook is wired.
func TestAccountCloseoutFinalPublisherPayout(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "closeout-pub")

	// Real revenue on this publisher's inventory (high CPM so the net is
	// meaningful in cents — see the payout-runner test for the 0.0035 rounding trap).
	auc := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "closeout-pub-user")
	win := h.ExtractWinner(t, auc)
	if win.NoBid {
		t.Fatal("expected a winning bid")
	}
	for i := 0; i < 3; i++ {
		tr := fmt.Sprintf("closeout-pub-imp-%d-%d", time.Now().UnixNano(), i)
		h.FireImpression(t, tr, win.CampaignID, win.CreativeID,
			auc.PlacementID, auc.PublisherID, w.AdvAcc.ID, "USD", 1000.0)
	}
	harness.WaitFor(t, 30*time.Second, "publisher gross in ClickHouse", func() bool {
		return chGrossForPublisher(t, w.Publisher.ID) >= 3.0-0.001
	})

	chPort := startCHPortForward(t)

	// Owner closes the publisher account; backdate grace so it's due now.
	client := h.OwnerClient(t, w.PubAcc.ID)
	if st, _ := h.AccountClose(t, client); st != 200 {
		t.Fatalf("publisher close initiate status %d", st)
	}
	if _, err := h.DB.Exec(
		`UPDATE account_closure_requests SET grace_ends_at = now() - interval '1 hour'
		 WHERE account_id = $1::uuid AND status = 'grace'`, w.PubAcc.ID); err != nil {
		t.Fatalf("backdate grace: %v", err)
	}

	// Run close-out with ClickHouse wired (gross source) + the NOBYPASSRLS role.
	cmd := exec.Command("go", "run", "./cmd/account-closeout")
	cmd.Dir = "../.."
	cmd.Env = append(os.Environ(),
		"CLICKHOUSE_ADDR=127.0.0.1:"+chPort,
		"DATABASE_URL=postgres://adtech_app:adtech-app-local@localhost:5432/adtech?sslmode=disable")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("account-closeout failed: %v\n%s", err, out)
	}

	if got := accountStatus(t, h, w.PubAcc.ID); got != "closed" {
		t.Errorf("publisher account status = %q, want closed", got)
	}
	var payouts int
	var amount float64
	if err := h.DB.QueryRow(
		`SELECT count(*), COALESCE(SUM(amount),0) FROM payouts WHERE publisher_id = $1::uuid`,
		w.Publisher.ID).Scan(&payouts, &amount); err != nil {
		t.Fatalf("payouts query: %v", err)
	}
	if payouts < 1 || amount <= 0 {
		t.Errorf("final payout: count=%d amount=%v, want >=1 payout with positive amount", payouts, amount)
	}
}

// TestAccountCloseoutPublisherDeferredWhenCHDown locks the money-safety invariant:
// a publisher closure whose final payout can't be computed (ClickHouse — the gross
// source — is down) must NOT be stranded 'closed'; it stays 'grace' and is retried.
// Closeout only re-selects 'grace' closures, so closing without paying would drop
// the publisher's final payment forever.
func TestAccountCloseoutPublisherDeferredWhenCHDown(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "closeout-chdown")

	client := h.OwnerClient(t, w.PubAcc.ID)
	if st, _ := h.AccountClose(t, client); st != 200 {
		t.Fatalf("publisher close initiate status %d", st)
	}
	if _, err := h.DB.Exec(
		`UPDATE account_closure_requests SET grace_ends_at = now() - interval '1 hour'
		 WHERE account_id = $1::uuid AND status = 'grace'`, w.PubAcc.ID); err != nil {
		t.Fatalf("backdate grace: %v", err)
	}

	// Run close-out with ClickHouse UNREACHABLE (no port-forward; a dead addr) — the
	// publisher branch must defer (best-effort: logged + skipped, binary still exits 0).
	runCloseout := func(chAddr string) []byte {
		t.Helper()
		cmd := exec.Command("go", "run", "./cmd/account-closeout")
		cmd.Dir = "../.."
		cmd.Env = append(os.Environ(),
			"CLICKHOUSE_ADDR="+chAddr,
			"DATABASE_URL=postgres://adtech_app:adtech-app-local@localhost:5432/adtech?sslmode=disable")
		out, _ := cmd.CombinedOutput() // ignore exit: a deferral is logged, not fatal
		return out
	}
	// The deferral signal is the CLOSURE REQUEST status (accounts.status is
	// 'suspended' throughout grace; it only flips to 'closed' at closeout).
	closureStatus := func() string {
		t.Helper()
		var s string
		if err := h.DB.QueryRow(`SELECT status FROM account_closure_requests WHERE account_id = $1::uuid ORDER BY requested_at DESC LIMIT 1`, w.PubAcc.ID).Scan(&s); err != nil {
			t.Fatalf("closure status: %v", err)
		}
		return s
	}
	out := runCloseout("127.0.0.1:1") // nothing listening → CH unavailable
	if got := closureStatus(); got != "grace" {
		t.Errorf("CH-down: closure status = %q, want 'grace' (deferred, not stranded closed)\n%s", got, out)
	}
	var payouts int
	if err := h.DB.QueryRow(`SELECT count(*) FROM payouts WHERE publisher_id = $1::uuid`, w.Publisher.ID).Scan(&payouts); err != nil {
		t.Fatalf("payouts query: %v", err)
	}
	if payouts != 0 {
		t.Errorf("CH-down: %d payouts written, want 0 (nothing settled while deferred)", payouts)
	}

	// Recovery: with ClickHouse reachable, the same due closure now closes (proving
	// the CH-down state was a retriable deferral, not a permanent strand).
	chPort := startCHPortForward(t)
	runCloseout("127.0.0.1:" + chPort)
	if got := closureStatus(); got != "closed" {
		t.Errorf("after CH recovery: closure status = %q, want 'closed'", got)
	}
}
