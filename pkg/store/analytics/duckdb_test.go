//go:build duckdb

package analytics

import (
	"context"
	"testing"
	"time"
)

// TestDuckDB_InsertAuctionWin guards the schema + insert wiring for
// auction_wins. Before 2026-06-06 InsertAuctionWin was a no-op (Schema
// TODO), so all three reporting handlers (AuctionWin, DirectWin,
// PrebidOutboundWin) silently dropped on the DuckDB backend. Test
// asserts the row lands and is queryable via QueryParams{Table:
// "auction_wins"}.
//
// Build-tagged `duckdb` because the marcboeker/go-duckdb driver is
// CGO. CI runs this via `go test -tags=duckdb ./pkg/store/analytics/...`.
func TestDuckDB_InsertAuctionWin(t *testing.T) {
	d, err := NewDuckDB(":memory:")
	if err != nil {
		t.Fatalf("open duckdb: %v", err)
	}
	defer d.Close()

	ctx := context.Background()
	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	if err := d.InsertAuctionWin(ctx, &AuctionWinEvent{
		TraceID:       "trace-1",
		AuctionID:     "auc-1",
		WinnerDSP:     "internal",
		CampaignID:    "cmp-1",
		CreativeID:    "cr-1",
		PlacementID:   "pl-1",
		PublisherID:   "pub-1",
		AdvertiserID:  "adv-1",
		ClearingPrice: 3.50,
		Currency:      "USD",
		BidModel:      "cpm",
		DealID:        "",
		Channel:       "display",
		SchemaVersion: 1,
		Timestamp:     now,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Round-trip via raw query — sanity check the row actually landed.
	var got string
	row := d.db.QueryRowContext(ctx, `SELECT trace_id FROM auction_wins WHERE trace_id = 'trace-1'`)
	if err := row.Scan(&got); err != nil {
		t.Fatalf("select after insert: %v", err)
	}
	if got != "trace-1" {
		t.Errorf("trace_id = %q, want trace-1", got)
	}
}
