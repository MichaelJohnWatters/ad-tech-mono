//go:build e2e

// Publisher payout-runner end to end: real wins + impressions → gross revenue in
// ClickHouse → the runner applies the publisher's rev-share contract, gates on
// the minimum-payout threshold, and writes an idempotent payouts row. Drives the
// runner exactly the way the CronJob does — `go run ./cmd/payout-runner` — the
// money-OUT mirror of TestInvoiceRunner.
package e2e

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestPayoutRunnerGeneratesPublisherPayout(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "payout")

	// One auction gives us valid campaign/creative ids; then fire a few
	// impressions at a high explicit CPM so the accrued net is meaningful in
	// cents (a single $3.50-CPM impression books 0.0035 gross → net rounds to
	// $0.00). price is CPM; the tracker books CPM/1000 per impression, so
	// price=1000 → $1.00 gross each.
	auc := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "payout-user-1")
	win := h.ExtractWinner(t, auc)
	if win.NoBid {
		t.Fatal("expected a winning bid for the payout world")
	}
	const price = 1000.0
	const nImps = 3
	wantGross := float64(nImps) * price / 1000 // 3.0
	for i := 0; i < nImps; i++ {
		tr := fmt.Sprintf("payout-imp-%d-%d", time.Now().UnixNano(), i)
		h.FireImpression(t, tr, win.CampaignID, win.CreativeID,
			auc.PlacementID, auc.PublisherID, w.AdvAcc.ID, "USD", price)
	}
	// Wait until ALL of the gross has landed in ClickHouse (the runner's source),
	// so a straggler can't make the payout short.
	harness.WaitFor(t, 30*time.Second, "publisher gross revenue in ClickHouse", func() bool {
		return chGrossForPublisher(t, w.Publisher.ID) >= wantGross-0.001
	})

	// CH native isn't host-reachable; port-forward it for the host-run binary.
	chPort := startCHPortForward(t)

	today := time.Now().UTC().Format("2006-01-02")
	tomorrow := time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02")
	runPayoutRunner := func() {
		t.Helper()
		cmd := exec.Command("go", "run", "./cmd/payout-runner", "--period-start", today, "--period-end", tomorrow)
		cmd.Dir = "../.."
		cmd.Env = append(os.Environ(), "CLICKHOUSE_ADDR=127.0.0.1:"+chPort)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("payout-runner failed: %v\n%s", err, out)
		}
	}
	countPayouts := func() (n int, amount, fee float64) {
		t.Helper()
		if err := h.DB.QueryRow(
			`SELECT count(*), COALESCE(SUM(amount),0), COALESCE(SUM(platform_fee),0)
			 FROM payouts WHERE publisher_id = $1::uuid AND period_start = $2::date`,
			w.Publisher.ID, today).Scan(&n, &amount, &fee); err != nil {
			t.Fatalf("payouts query: %v", err)
		}
		return n, amount, fee
	}

	// 1) MINIMUM-THRESHOLD GATE: a payout method whose minimum ($1000) is well
	//    above this period's ~$2.40 net → the runner must HOLD (write nothing).
	if _, err := h.DB.Exec(
		`INSERT INTO payout_methods (account_id, method_type, minimum_payout_cents, currency, status)
		 VALUES ($1::uuid, 'bank_transfer', 100000, 'USD', 'active')`, w.PubAcc.ID); err != nil {
		t.Fatalf("seed payout method (high minimum): %v", err)
	}
	runPayoutRunner()
	if n, _, _ := countPayouts(); n != 0 {
		t.Fatalf("payout written despite net below the $1000 minimum: got %d rows, want 0 (held)", n)
	}

	// 2) Drop the minimum → the runner writes the payout. net + platform_fee must
	//    reconstruct the gross, proving the rev-share split is applied + money-complete.
	if _, err := h.DB.Exec(
		`UPDATE payout_methods SET minimum_payout_cents = 0 WHERE account_id = $1::uuid`, w.PubAcc.ID); err != nil {
		t.Fatalf("lower minimum: %v", err)
	}
	runPayoutRunner()
	n, amount, fee := countPayouts()
	if n != 1 {
		t.Fatalf("payouts = %d, want exactly 1 after clearing the minimum", n)
	}
	if amount <= 0 || fee <= 0 {
		t.Errorf("payout amount=%v fee=%v, want both > 0 (a real rev-share split)", amount, fee)
	}
	if amount <= fee {
		t.Errorf("net %v <= platform_fee %v — the publisher should keep the majority at a 20%% fee", amount, fee)
	}
	if got := amount + fee; got < wantGross-0.02 || got > wantGross+0.02 {
		t.Errorf("amount+platform_fee = %v, want ~= gross %v (net+margin must reconstruct gross)", got, wantGross)
	}

	// 3) IDEMPOTENCY: a re-run for the same publisher+period must not duplicate
	//    (uq_payouts_publisher_period + the pending-only upsert).
	runPayoutRunner()
	if n2, _, _ := countPayouts(); n2 != 1 {
		t.Errorf("after re-run payouts = %d, want still 1 (idempotent on publisher+period)", n2)
	}
}

// chGrossForPublisher returns the summed clearing_price_usd for a publisher from
// ClickHouse via kubectl exec (CH native isn't host-reachable outside the
// per-test port-forward, and this runs before it is set up).
func chGrossForPublisher(t *testing.T, publisherID string) float64 {
	t.Helper()
	q := fmt.Sprintf("SELECT sum(clearing_price_usd) FROM adtech.impressions WHERE publisher_id='%s'", publisherID)
	out, err := exec.Command("kubectl", "-n", "adtech", "exec", "-i", "clickhouse-0", "--",
		"clickhouse-client", "-q", q).Output()
	if err != nil {
		return 0
	}
	v, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	return v
}

// startCHPortForward forwards the in-cluster ClickHouse native port to a host
// port for the test's duration, returning the host port. Cleaned up via t.Cleanup.
func startCHPortForward(t *testing.T) string {
	t.Helper()
	const hostPort = "19000"
	cmd := exec.Command("kubectl", "-n", "adtech", "port-forward", "clickhouse-0", hostPort+":9000")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start clickhouse port-forward: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:"+hostPort, time.Second)
		if err == nil {
			_ = conn.Close()
			return hostPort
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("clickhouse port-forward never became reachable on :%s", hostPort)
	return ""
}
