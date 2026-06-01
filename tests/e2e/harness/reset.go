//go:build e2e

package harness

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// Reset wipes all tenant state so the test starts from a known empty world.
//
// Specifically:
//   - TRUNCATE every tenant table (accounts cascade through the FK graph)
//   - FLUSHDB on Redis (budget counters, freq caps, dedup)
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
		"targeting_rules",
		"line_items",
		"creatives",
		"insertion_orders",
		"placements",
		"deals",
		"publishers",
		"audience_segment_members",
		"audience_segments",
		"budget_reservations",
		"ledger_entries",
		"invoices",
		"payouts",
		"adjustments",
		"advertiser_balances",
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

	t.Log("harness reset complete")
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
