//go:build duckdb

package analytics

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestDuckDB_SurvivesReopen is the test that justifies the whole DuckDB
// backend: events written to the file must still be there after the store
// is closed and reopened (i.e. after a reporting pod restart). The memory
// backend fails this by construction — that's the gap A1 closes.
func TestDuckDB_SurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "persist.duckdb")
	ctx := context.Background()
	ts := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)

	// First "pod lifetime": write three impressions and a win, then close.
	d1, err := NewDuckDB(path)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := d1.InsertImpression(ctx, &ImpressionEvent{
			TraceID: "t-persist", CampaignID: "c1", CreativeID: "cr1",
			PlacementID: "p1", PublisherID: "pub1", AccountID: "acct1",
			ClearingPrice: 2.50, ClearingCurrency: "USD", ClearingPriceUSD: 2.50,
			SchemaVersion: 1, Timestamp: ts,
		}); err != nil {
			t.Fatalf("insert impression: %v", err)
		}
	}
	if err := d1.InsertAuctionWin(ctx, &AuctionWinEvent{
		TraceID: "t-persist", CampaignID: "c1", PlacementID: "p1",
		ClearingPrice: 2.50, BidModel: "cpm", SchemaVersion: 1, Timestamp: ts,
	}); err != nil {
		t.Fatalf("insert win: %v", err)
	}
	if err := d1.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}

	// Second "pod lifetime": reopen the same file, the rows must be there.
	d2, err := NewDuckDB(path)
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	defer d2.Close()

	res, err := d2.Query(ctx, QueryParams{
		Table:   "impressions",
		Metrics: []string{"count"},
		Filters: map[string]string{"trace_id": "t-persist"},
	})
	if err != nil {
		t.Fatalf("query after reopen: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("got %d result rows, want 1: %+v", len(res.Rows), res.Rows)
	}
	got := res.Rows[0][0]
	if n, ok := toInt64(got); !ok || n != 3 {
		t.Fatalf("impression count after reopen = %v (%T), want 3", got, got)
	}
}

// TestDuckDB_OperationalSignalsSurviveReopen proves the operational-signal
// writes (ObservabilityWriter) land in real tables and survive a restart —
// on the memory backend these are slices lost on exit; on DuckDB they must
// persist like any other event.
func TestDuckDB_OperationalSignalsSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signals.duckdb")
	ts := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)

	d1, err := NewDuckDB(path)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	d1.InsertFreqCapBlock(FreqCapBlock{TraceID: "t1", UserID: "u1", CampaignID: "c1", Timestamp: ts})
	d1.InsertFreqCapBlock(FreqCapBlock{TraceID: "t2", UserID: "u2", CampaignID: "c1", Timestamp: ts})
	d1.InsertServeNoFill(ServeNoFill{TraceID: "t3", PublisherID: "pub1", Reason: "no-fill", Timestamp: ts})
	d1.InsertBudgetDepletion(BudgetDepletion{CampaignID: "c1", Budget: 100, Spent: 100, Timestamp: ts})
	if err := d1.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}

	d2, err := NewDuckDB(path)
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	defer d2.Close()

	// Same-package test can read the unexported handle directly.
	for _, tc := range []struct {
		table string
		want  int
	}{
		{"freq_cap_blocks", 2},
		{"serve_no_fills", 1},
		{"budget_depletions", 1},
	} {
		var n int
		if err := d2.db.QueryRow("SELECT count(*) FROM " + tc.table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tc.table, err)
		}
		if n != tc.want {
			t.Errorf("%s rows after reopen = %d, want %d", tc.table, n, tc.want)
		}
	}
}

// toInt64 normalises whatever numeric type the driver returns for a
// COUNT(*) so the assertion isn't coupled to DuckDB's scan type.
func toInt64(v interface{}) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int32:
		return int64(n), true
	case int:
		return int64(n), true
	default:
		return 0, false
	}
}
