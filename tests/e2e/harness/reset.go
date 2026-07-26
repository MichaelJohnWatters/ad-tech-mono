//go:build e2e

package harness

import (
	"context"
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
	if _, err := h.DB.ExecContext(ctx, stmt); err != nil {
		t.Fatalf("truncate: %v", err)
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
	client := &http.Client{Timeout: 5 * time.Second}
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
