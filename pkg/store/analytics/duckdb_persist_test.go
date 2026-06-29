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
