//go:build e2e

// Invoice-runner end to end: settled spend → invoice rows. The runner
// shipped with zero e2e coverage (monthly cron + host one-off); this
// drives it exactly the way ops does — `go run ./cmd/invoice-runner` —
// against real settled spend from a real won auction, and proves the
// idempotency key (account+period) holds on a second run.
package e2e

import (
	"os/exec"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestInvoiceRunnerGeneratesInvoiceFromSettledSpend(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "invoice")

	// One CPM win + impression = immediate settle in the billing engine,
	// which the spend snapshot flow lands in campaign_committed_spend.
	auc := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "invoice-user-1")
	win := h.ExtractWinner(t, auc)
	if win.NoBid {
		t.Fatal("expected a winning bid for the invoice world")
	}
	h.FireImpression(t, auc.TraceID, win.CampaignID, win.CreativeID,
		auc.PlacementID, auc.PublisherID, w.AdvAcc.ID, "USD", win.Price)

	var settledMicros int64
	// 60s: settle rides win event → NATS → billing engine → Postgres write,
	// which lags under full-suite consumer load (30s flaked in suite runs).
	harness.WaitFor(t, 60*time.Second, "settled spend to land in campaign_committed_spend", func() bool {
		err := h.DB.QueryRow(
			`SELECT COALESCE(SUM(settled_micros),0) FROM campaign_committed_spend WHERE campaign_id = $1`,
			win.CampaignID).Scan(&settledMicros)
		return err == nil && settledMicros > 0
	})

	// Run the invoice runner for today's period, exactly like ops would.
	today := time.Now().UTC().Format("2006-01-02")
	tomorrow := time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02")
	runInvoiceRunner := func() {
		t.Helper()
		cmd := exec.Command("go", "run", "./cmd/invoice-runner",
			"--account", w.AdvAcc.ID, "--period-start", today, "--period-end", tomorrow)
		cmd.Dir = "../.."
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("invoice-runner failed: %v\n%s", err, out)
		}
	}
	runInvoiceRunner()

	var invoices int
	var total float64
	if err := h.DB.QueryRow(
		`SELECT count(*), COALESCE(SUM(total),0) FROM invoices WHERE account_id = $1 AND period_start = $2::date`,
		w.AdvAcc.ID, today).Scan(&invoices, &total); err != nil {
		t.Fatalf("invoices query: %v", err)
	}
	if invoices != 1 {
		t.Fatalf("invoices = %d, want exactly 1", invoices)
	}
	// micros → dollars, exactly (money precision: settled_micros is the truth).
	want := float64(settledMicros) / 1_000_000
	if total < want-0.000001 || total > want+0.000001 {
		t.Errorf("invoice total = %v, want %v (settled %d micros)", total, want, settledMicros)
	}

	var lineItems int
	if err := h.DB.QueryRow(
		`SELECT count(*) FROM invoice_line_items ili JOIN invoices i ON i.id = ili.invoice_id
		 WHERE i.account_id = $1 AND ili.campaign_id = $2`,
		w.AdvAcc.ID, win.CampaignID).Scan(&lineItems); err != nil {
		t.Fatalf("line items query: %v", err)
	}
	if lineItems != 1 {
		t.Errorf("invoice_line_items for campaign = %d, want 1", lineItems)
	}

	// Idempotency: a second run for the same account+period must not
	// create a second invoice.
	runInvoiceRunner()
	if err := h.DB.QueryRow(
		`SELECT count(*) FROM invoices WHERE account_id = $1 AND period_start = $2::date`,
		w.AdvAcc.ID, today).Scan(&invoices); err != nil {
		t.Fatalf("invoices recount: %v", err)
	}
	if invoices != 1 {
		t.Errorf("after re-run invoices = %d, want still 1 (idempotent on account+period)", invoices)
	}
}
