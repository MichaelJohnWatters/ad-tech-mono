//go:build e2e

// 90-day account purge (the last account-closure deferral): once a closed
// account's retention window elapses, account-closeout destructively removes its
// data across Postgres (every account-keyed table) + ClickHouse (analytics) and
// marks the closure 'purged'. The accounts row + closure record survive as a
// tombstone. This drives it exactly as the daily cron does.
package e2e

import (
	"context"
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

	// A completed data-export job + its ZIP in the private reports bucket — the zip
	// is a full copy of the account's exported data, so the purge MUST delete the
	// object, not just the pointer row (GDPR completeness + no orphaned blob).
	reportsStore, reportsBucket := h.ReportsBucket(t)
	exportKey := "exports/purge-e2e-" + adv.ID + ".zip"
	const exportBody = "PK\x03\x04 fake export zip"
	if err := reportsStore.Put(context.Background(), reportsBucket, exportKey,
		strings.NewReader(exportBody), int64(len(exportBody)), "application/zip"); err != nil {
		t.Fatalf("put export zip: %v", err)
	}
	if _, err := h.DB.Exec(
		`INSERT INTO account_export_jobs (account_id, status, artifact_bucket, artifact_key, artifact_bytes, finished_at)
		 VALUES ($1::uuid, 'done', $2, $3, 22, now())`, adv.ID, reportsBucket, exportKey); err != nil {
		t.Fatalf("seed export job: %v", err)
	}

	// A transcoded SSAI segment under the account's OWN creative (keyed by creative
	// id, ssai/cond/{creativeID}/…) — the one account-owned creative blob that exists
	// today. The purge MUST delete it (tenant-safe; shared theme assets are left).
	var creativeID string
	if err := h.DB.QueryRow(`SELECT id::text FROM creatives WHERE account_id = $1::uuid LIMIT 1`, adv.ID).Scan(&creativeID); err != nil {
		t.Fatalf("creative id: %v", err)
	}
	const creativesBucket = "adtech-creatives"
	segPrefix := "ssai/cond/" + creativeID
	segKey := segPrefix + "/deadbeef/index.m3u8"
	if err := reportsStore.Put(context.Background(), creativesBucket, segKey,
		strings.NewReader("#EXTM3U"), int64(len("#EXTM3U")), "application/x-mpegURL"); err != nil {
		t.Fatalf("put transcoded segment: %v", err)
	}
	harness.WaitFor(t, 30*time.Second, "impression in ClickHouse", func() bool {
		return chImpressionsForAccount(t, adv.ID) >= 1
	})

	// A DIFFERENT account (the publisher) with its OWN audience segment + marketplace
	// listing — purging the advertiser is account-scoped and must NOT touch another
	// tenant's data (locks the isolation guarantee + the decision to keep
	// marketplace_listings off the purge allowlist).
	pub := w.PubAcc
	var pubSeg string
	if err := h.DB.QueryRow(
		`INSERT INTO audience_segments (account_id, name, type) VALUES ($1::uuid, 'pub-own-seg', 'first_party') RETURNING id::text`,
		pub.ID).Scan(&pubSeg); err != nil {
		t.Fatalf("seed publisher segment: %v", err)
	}
	if _, err := h.DB.Exec(
		`INSERT INTO marketplace_listings (account_id, segment_id, name) VALUES ($1::uuid, $2::uuid, 'pub-own-listing')`,
		pub.ID, pubSeg); err != nil {
		t.Fatalf("seed publisher listing: %v", err)
	}

	chPort := startCHPortForward(t)
	runCloseout := func() {
		t.Helper()
		cmd := exec.Command("go", "run", "./cmd/account-closeout")
		cmd.Dir = "../.."
		cmd.Env = append(os.Environ(),
			"CLICKHOUSE_ADDR=127.0.0.1:"+chPort,
			"DATABASE_URL=postgres://adtech_app:adtech-app-local@localhost:5432/adtech?sslmode=disable",
			// Reach the real Minio so the purge deletes the export ZIP (else the
			// binary falls back to a local fs store and the object survives).
			"S3_ENDPOINT="+h.URLs.MinioEndpt,
			"S3_ACCESS_KEY=adtech",
			"S3_SECRET_KEY=adtech-local-dev")
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

	// The export ZIP object is gone from the reports bucket (not just its PG row).
	if exists, err := reportsStore.Exists(context.Background(), reportsBucket, exportKey); err != nil {
		t.Fatalf("export zip exists check: %v", err)
	} else if exists {
		t.Errorf("export zip %q still in bucket after purge (object-storage blob not purged)", exportKey)
	}

	// The account's transcoded SSAI creative segments are gone from the bucket.
	if segs, err := reportsStore.List(context.Background(), creativesBucket, segPrefix); err != nil {
		t.Fatalf("list segments: %v", err)
	} else if len(segs) != 0 {
		t.Errorf("transcoded creative segments after purge = %d, want 0 (creative blobs not purged): %v", len(segs), segs)
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

	// Tenant isolation: the OTHER account's own segment + marketplace listing are
	// untouched by the advertiser's purge.
	var pubSegAfter, pubListingAfter int
	h.DB.QueryRow(`SELECT count(*) FROM audience_segments WHERE account_id = $1::uuid`, pub.ID).Scan(&pubSegAfter)
	h.DB.QueryRow(`SELECT count(*) FROM marketplace_listings WHERE account_id = $1::uuid`, pub.ID).Scan(&pubListingAfter)
	if pubSegAfter < 1 || pubListingAfter < 1 {
		t.Errorf("advertiser purge deleted the publisher's own data: segments=%d listings=%d (want >=1 each — cross-tenant over-delete)", pubSegAfter, pubListingAfter)
	}

	// Idempotent: the closure is now 'purged', not 'closed', so a re-run selects
	// nothing, does not error, and does NOT re-stamp purged_at.
	firstPurgedAt := purgedAt
	runCloseout()
	var status2 string
	var purgedAt2 *time.Time
	if err := h.DB.QueryRow(
		`SELECT status, purged_at FROM account_closure_requests WHERE account_id = $1::uuid ORDER BY requested_at DESC LIMIT 1`,
		adv.ID).Scan(&status2, &purgedAt2); err != nil {
		t.Fatalf("closure row after re-run: %v", err)
	}
	if status2 != "purged" {
		t.Errorf("closure status after re-run = %q, want purged (idempotent)", status2)
	}
	if firstPurgedAt != nil && purgedAt2 != nil && !firstPurgedAt.Equal(*purgedAt2) {
		t.Errorf("purged_at changed on re-run: %v → %v (not idempotent)", *firstPurgedAt, *purgedAt2)
	}
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
