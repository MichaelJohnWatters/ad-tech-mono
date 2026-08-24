//go:build e2e

package harness

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// Reset wipes all tenant state so the test starts from a known empty world.
//
// Specifically:
//   - TRUNCATE every tenant table (accounts cascade through the FK graph)
//   - FLUSHDB on Redis (budget counters, freq caps, dedup)
//   - TRUNCATE every ClickHouse analytics table (the real hot store — the
//     harness read-backs count rows there, so leftover events from earlier
//     runs would poison fixed-trace assertions). Best-effort: skipped when
//     ClickHouse isn't reachable (e.g. a memory-backend stack).
//   - Skip Minio for now — creative bodies are tiny and Reset would need
//     bucket enumeration; the seed UPSERT is fine for object storage.
//
// We don't run migrations here — the test assumes `make migrate` has already
// happened (Tilt does this automatically). Resetting between tests is fast
// (<200ms locally) so each TestEndToEnd run starts clean.
func (h *Harness) Reset(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Tables in dependency order — TRUNCATE ... CASCADE handles FKs, but
	// listing them out keeps the intent explicit and surfaces newly-added
	// tables when a future migration breaks the test.
	tables := []string{
		"line_item_creatives",
		"publisher_line_item_creatives",
		"targeting_rules",
		"line_items",
		"publisher_line_items",
		"creatives",
		"insertion_orders",
		"placements",
		"deals",
		"publishers",
		"audience_segment_members",
		"audience_segments",
		// Global (no account FK, so not reached by CASCADE from accounts) —
		// list explicitly so privacy tests get clean opt-out / identity state.
		"opt_out_registry",
		"identity_graph",
		"budget_reservations",
		"reservation_context",
		"campaign_committed_spend",
		"ledger_entries",
		// Data monetization (ADR 0009): parked attributions, accrued
		// earnings, and seat receivables are money state — leftovers would
		// skew later tests' balance/ledger deltas.
		"data_fee_pending",
		"data_fee_earnings",
		"data_fee_receivables",
		"invoices",
		"payouts",
		"adjustments",
		"advertiser_balances",
		"topups",
		"api_keys",
		"team_members",
		"accounts",
	}
	stmt := "TRUNCATE TABLE " + commaJoin(tables) + " RESTART IDENTITY CASCADE"
	// Retry on deadlock: TRUNCATE takes an ACCESS EXCLUSIVE lock on every listed
	// table, so a background writer touching any of them mid-suite (billing →
	// ledger_entries, a cron → invoices/payouts, audience-rt →
	// audience_segment_members) can deadlock it (pq 40P01). A deadlock is
	// transient by design — Postgres aborts one side (us) and the other completes
	// — so a short retry almost always succeeds. Without this, a random ~1-per-run
	// test flaked on "truncate: pq: deadlock detected".
	if err := execWithDeadlockRetry(ctx, h.DB.ExecContext, stmt); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	// Clean per-advertiser secrets (account-scoped, e.g. G7 hmac_conversion keys).
	// The secrets table isn't truncated (platform api_key / jwt / hmac_tracker must
	// survive a reset — migration 072 dropped the accounts FK so the truncate no
	// longer cascades here). But a per-advertiser key keyed on a DETERMINISTIC
	// account id (the harness's adv-acme) would otherwise persist and re-associate
	// when the next test recreates that account — so under strict its conversions
	// (signed with the platform key) would 403. Delete them so harness advertisers
	// start keyless (platform-key fallback); the reseed re-mints for seed advertisers.
	if err := execWithDeadlockRetry(ctx, h.DB.ExecContext, `DELETE FROM secrets WHERE account_id IS NOT NULL`); err != nil {
		t.Fatalf("clean per-advertiser secrets: %v", err)
	}

	// Redis FLUSHDB — keep this fast and silent. Failure is logged but not
	// fatal so we don't block the test on a transient Redis blip.
	rdb := redis.NewClient(&redis.Options{Addr: h.URLs.RedisAddr})
	defer rdb.Close()
	if err := rdb.FlushDB(ctx).Err(); err != nil {
		t.Logf("Reset: redis FLUSHDB failed (continuing): %v", err)
	}

	h.resetClickHouse(t, ctx)

	t.Log("harness reset complete")
}

// resetClickHouse truncates every adtech.* table via the ClickHouse HTTP
// interface (h.URLs.ClickHouseHTTP; Tilt forwards it). Best-effort by design:
// on a stack without ClickHouse (memory backend, CI) the port isn't there and
// we skip silently.
func (h *Harness) resetClickHouse(t *testing.T, ctx context.Context) {
	t.Helper()
	tables, err := h.clickhouseQuery(ctx, "SHOW TABLES FROM adtech")
	if err != nil {
		t.Logf("Reset: clickhouse unreachable, skipping analytics truncate: %v", err)
		return
	}
	for _, table := range strings.Fields(tables) {
		if _, err := h.clickhouseQuery(ctx, "TRUNCATE TABLE IF EXISTS adtech.`"+table+"`"); err != nil {
			t.Logf("Reset: clickhouse truncate %s failed (continuing): %v", table, err)
		}
	}
}

func (h *Harness) clickhouseQuery(ctx context.Context, query string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URLs.ClickHouseHTTP, strings.NewReader(query))
	if err != nil {
		return "", err
	}
	client := newHTTPClient(5 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("clickhouse %q: status %d: %s", query, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return string(body), nil
}

func commaJoin(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += ", " + p
	}
	return out
}

// execWithDeadlockRetry runs a statement, retrying on a Postgres deadlock
// (SQLSTATE 40P01) or serialization failure (40001). Both are transient by
// design — Postgres aborts one side of the conflict and the other completes —
// so a short bounded retry clears them. Used for the reset TRUNCATE/DELETE,
// which take table-level locks that a background writer (billing, crons,
// audience-rt) can briefly conflict with mid-suite. Matches on the pq error
// text to avoid taking a lib/pq type dependency in the harness.
func execWithDeadlockRetry(ctx context.Context, exec func(context.Context, string, ...any) (sql.Result, error), stmt string) error {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		if _, err = exec(ctx, stmt); err == nil {
			return nil
		}
		msg := err.Error()
		if !strings.Contains(msg, "deadlock detected") && !strings.Contains(msg, "40P01") &&
			!strings.Contains(msg, "could not serialize") && !strings.Contains(msg, "40001") {
			return err // not a retryable lock conflict
		}
		// Short backoff — the conflicting txn just needs to finish.
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Duration(attempt+1) * 150 * time.Millisecond):
		}
	}
	return err
}
