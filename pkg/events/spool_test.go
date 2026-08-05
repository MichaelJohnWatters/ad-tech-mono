package events

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// failingBus fails every publish until healed, then records what landed.
type failingBus struct {
	mu        sync.Mutex
	healthy   bool
	published []string // msgIDs in arrival order
}

func (b *failingBus) Subscribe(context.Context, string, string, Handler) error { return nil }
func (b *failingBus) Close() error                                             { return nil }

func (b *failingBus) heal() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.healthy = true
}

func (b *failingBus) ids() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.published...)
}

func (b *failingBus) Publish(ctx context.Context, subject string, data []byte) error {
	return b.PublishWithID(ctx, subject, "", data)
}

func (b *failingBus) PublishWithID(ctx context.Context, subject, msgID string, data []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.healthy {
		return errors.New("nats down")
	}
	b.published = append(b.published, msgID)
	return nil
}

func TestSpoolAbsorbsAndDrains(t *testing.T) {
	bus := &failingBus{}
	pub := NewPublisher(bus, testLogger())
	spool, err := NewSpool(t.TempDir(), 1<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	stop := pub.EnableSpool(context.Background(), spool)
	defer stop()

	// Bus down: publishes are ABSORBED (nil error), spooled, pressure rises.
	for i := 0; i < 50; i++ {
		if err := pub.AuctionWin(context.Background(), AuctionWinEvent{TraceID: fmt.Sprintf("t%03d", i)}); err != nil {
			t.Fatalf("publish %d should be absorbed by the spool, got %v", i, err)
		}
	}
	if spool.Pressure() == 0 {
		t.Fatal("pressure should be nonzero with 50 spooled events")
	}
	if got := len(bus.ids()); got != 0 {
		t.Fatalf("nothing should have reached the bus, got %d", got)
	}

	// Bus heals: the drainer republishes everything with original msg IDs.
	bus.heal()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(bus.ids()) == 50 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	ids := bus.ids()
	if len(ids) != 50 {
		t.Fatalf("expected 50 drained events, got %d", len(ids))
	}
	if ids[0] != "win:t000" || ids[49] != "win:t049" {
		t.Fatalf("drain must preserve original msg ids in order, got first=%s last=%s", ids[0], ids[49])
	}
	if p := spool.Pressure(); p != 0 {
		t.Fatalf("spool should be empty after drain, pressure=%d", p)
	}
}

func TestSpoolCapDropsAndCounts(t *testing.T) {
	spool, err := NewSpool(t.TempDir(), 200, nil) // tiny cap
	if err != nil {
		t.Fatal(err)
	}
	if !spool.Append("s", "id1", []byte("x")) {
		t.Fatal("first append should fit")
	}
	big := make([]byte, 400)
	if spool.Append("s", "id2", big) {
		t.Fatal("append past cap must report drop")
	}
}

func TestSpoolResumesAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	s1, err := NewSpool(dir, 1<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	s1.Append("subj", "keep-me", []byte("payload"))

	// Simulate a container restart: reopen the same dir.
	s2, err := NewSpool(dir, 1<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Pressure() == 0 {
		t.Fatal("reopened spool should still hold the undrained event")
	}
	var got []string
	n, err := s2.drainBatch(context.Background(), 10, func(_ context.Context, subject, msgID string, data []byte) error {
		got = append(got, msgID)
		return nil
	})
	if err != nil || n != 1 || len(got) != 1 || got[0] != "keep-me" {
		t.Fatalf("expected the surviving event to drain, n=%d err=%v got=%v", n, err, got)
	}
}
