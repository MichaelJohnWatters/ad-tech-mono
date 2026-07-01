package main

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/datalake"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestDatalakeSink_BatchFlush(t *testing.T) {
	lake := datalake.NewMemory(quietLog())
	sink := newDatalakeSink(lake, 2, quietLog()) // flush every 2 records
	ctx := context.Background()

	// One record — under the batch size, not flushed yet.
	sink.record("impressions", datalake.Record{"trace_id": "t1", "campaign_id": "c1"})
	if recs, _ := lake.Read(ctx, "impressions", datalake.Filter{}); len(recs) != 0 {
		t.Fatalf("expected 0 flushed before batch fills, got %d", len(recs))
	}

	// Second record hits the batch size — auto-flush.
	sink.record("impressions", datalake.Record{"trace_id": "t2", "campaign_id": "c1"})
	recs, err := lake.Read(ctx, "impressions", datalake.Filter{})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("expected 2 flushed at batch size, got %d", len(recs))
	}
}

func TestDatalakeSink_FlushDrainsPartialBuffers(t *testing.T) {
	lake := datalake.NewMemory(quietLog())
	sink := newDatalakeSink(lake, 100, quietLog()) // large batch — nothing auto-flushes
	ctx := context.Background()

	sink.record("impressions", datalake.Record{"trace_id": "t1"})
	sink.record("clicks", datalake.Record{"trace_id": "t2"})

	// Nothing flushed yet.
	if recs, _ := lake.Read(ctx, "impressions", datalake.Filter{}); len(recs) != 0 {
		t.Fatalf("expected nothing flushed before Flush, got %d", len(recs))
	}

	// Flush (as shutdown/ticker would) drains every table.
	sink.Flush(ctx)
	if recs, _ := lake.Read(ctx, "impressions", datalake.Filter{}); len(recs) != 1 {
		t.Errorf("impressions after Flush = %d, want 1", len(recs))
	}
	if recs, _ := lake.Read(ctx, "clicks", datalake.Filter{}); len(recs) != 1 {
		t.Errorf("clicks after Flush = %d, want 1", len(recs))
	}
}

// Snapshot must reconcile to the exact number of events fed in — the on-disk
// "zero data slippage" guarantee that the /debug/datalake/snapshot endpoint
// surfaces. Snapshot() flushes first, so the count is immediately consistent
// across the size-flush + buffered boundary.
func TestDatalakeSink_SnapshotReconciles(t *testing.T) {
	lake := datalake.NewMemory(quietLog())
	sink := newDatalakeSink(lake, 10, quietLog()) // size-flush every 10
	ctx := context.Background()

	const n = 23 // 20 auto-flushed + 3 still buffered
	for i := 0; i < n; i++ {
		sink.record("impressions", datalake.Record{"trace_id": "t", "campaign_id": "c1"})
	}
	snap, err := sink.Snapshot(ctx, "impressions")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.TotalRows != n {
		t.Fatalf("Parquet snapshot TotalRows = %d, want %d (every event lands)", snap.TotalRows, n)
	}
}
