package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/datalake"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// erroringLake wraps a MemoryStore but fails every Write — to exercise the
// flush-failure path.
type erroringLake struct {
	*datalake.MemoryStore
}

func (e *erroringLake) Write(context.Context, string, []datalake.Record, datalake.Schema) error {
	return fmt.Errorf("simulated lake write failure")
}

// TestDatalakeSink_AckAfterFlush: a buffered event is NOT acked until it is
// durably flushed — the guarantee that prevents losing acked-but-unwritten
// events on a crash/restart.
func TestDatalakeSink_AckAfterFlush(t *testing.T) {
	sink := newDatalakeSink(datalake.NewMemory(quietLog()), 2, quietLog()) // flush every 2
	var acks, naks int
	ack := func() error { acks++; return nil }
	nak := func() error { naks++; return nil }

	// One event: buffered, under batch size → not flushed → NOT acked yet.
	sink.bufferEvent("impressions", bufferedEvent{rec: datalake.Record{"trace_id": "t1"}, ack: ack, nak: nak})
	if acks != 0 {
		t.Fatalf("acked before durable flush: acks=%d, want 0", acks)
	}

	// Second event hits the batch size → flush → both acked, none naked.
	sink.bufferEvent("impressions", bufferedEvent{rec: datalake.Record{"trace_id": "t2"}, ack: ack, nak: nak})
	if acks != 2 || naks != 0 {
		t.Fatalf("after flush acks=%d naks=%d, want 2/0", acks, naks)
	}
}

// TestDatalakeSink_NakOnWriteFailure: when the durable write fails, events are
// NAKed (so JetStream redelivers) and never acked — at-least-once, no silent
// data loss.
func TestDatalakeSink_NakOnWriteFailure(t *testing.T) {
	sink := newDatalakeSink(&erroringLake{datalake.NewMemory(quietLog())}, 100, quietLog())
	var acks, naks int
	for i := 0; i < 3; i++ {
		sink.bufferEvent("impressions", bufferedEvent{
			rec: datalake.Record{"trace_id": "t"},
			ack: func() error { acks++; return nil },
			nak: func() error { naks++; return nil },
		})
	}
	sink.Flush(context.Background()) // Write fails inside

	if acks != 0 || naks != 3 {
		t.Fatalf("on write failure acks=%d naks=%d, want 0/3 (redeliver, never lose)", acks, naks)
	}
}

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
