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

// TestDuckDB_InsertMediaEvent guards the media_events writer + schema.
// Before this lands, video / audio events arriving at reporting hit a
// type-assert against *MemoryStore and silently dropped when the
// backend was DuckDB. Test asserts a video event lands and an audio
// event lands, and that filtering by channel works.
func TestDuckDB_InsertMediaEvent(t *testing.T) {
	d, err := NewDuckDB(":memory:")
	if err != nil {
		t.Fatalf("open duckdb: %v", err)
	}
	defer d.Close()

	ctx := context.Background()
	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)

	for _, e := range []*MediaEvent{
		{TraceID: "trace-1", Channel: "video", EventType: "firstQuartile", PositionMs: 3750, Timestamp: now},
		{TraceID: "trace-1", Channel: "video", EventType: "complete", PositionMs: 15000, Timestamp: now},
		{TraceID: "trace-2", Channel: "audio", EventType: "complete", PositionMs: 30000, Timestamp: now},
	} {
		if err := d.InsertMediaEvent(ctx, e); err != nil {
			t.Fatalf("insert %s/%s: %v", e.Channel, e.EventType, err)
		}
	}

	// Total count per channel.
	var videoCount, audioCount int
	if err := d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM media_events WHERE channel = 'video'`).Scan(&videoCount); err != nil {
		t.Fatalf("count video: %v", err)
	}
	if err := d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM media_events WHERE channel = 'audio'`).Scan(&audioCount); err != nil {
		t.Fatalf("count audio: %v", err)
	}
	if videoCount != 2 || audioCount != 1 {
		t.Errorf("video=%d audio=%d, want 2 / 1", videoCount, audioCount)
	}

	// Event-type filter — the reporting service's queries will look
	// for the right event_type when computing quartile completion
	// rates. Pin that the value round-trips intact.
	var ev string
	err = d.db.QueryRowContext(ctx,
		`SELECT event_type FROM media_events WHERE trace_id = 'trace-1' AND event_type = 'firstQuartile'`).Scan(&ev)
	if err != nil {
		t.Fatalf("select firstQuartile: %v", err)
	}
	if ev != "firstQuartile" {
		t.Errorf("event_type = %q, want firstQuartile", ev)
	}

	// Nil event must be a no-op (defensive — reporting handlers should
	// never pass nil, but the implementation guards it). Asserts no
	// new row is written.
	if err := d.InsertMediaEvent(ctx, nil); err != nil {
		t.Errorf("nil event returned error: %v", err)
	}
	var total int
	if err := d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM media_events`).Scan(&total); err != nil {
		t.Fatalf("count after nil: %v", err)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3 (nil event must not insert)", total)
	}
}
