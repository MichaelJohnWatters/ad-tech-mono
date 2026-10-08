package main

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// testBurlNotifier builds a notifier whose flushes land in memory — the
// batching loop runs for real, the database does not (db stays nil: the
// sweep ticker can't fire inside a test's lifetime).
func testBurlNotifier(t *testing.T) (*burlNotifier, func() [][]parkedBurl) {
	t.Helper()
	var mu sync.Mutex
	var flushes [][]parkedBurl
	b := &burlNotifier{
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		parked:  make(chan parkedBurl, parkBufferCap),
		stopped: make(chan struct{}),
		done:    make(chan struct{}),
	}
	b.flushFn = func(rows []parkedBurl) {
		mu.Lock()
		defer mu.Unlock()
		cp := make([]parkedBurl, len(rows))
		copy(cp, rows)
		flushes = append(flushes, cp)
	}
	go b.run()
	t.Cleanup(b.Stop)
	return b, func() [][]parkedBurl {
		mu.Lock()
		defer mu.Unlock()
		out := make([][]parkedBurl, len(flushes))
		copy(out, flushes)
		return out
	}
}

func countRows(flushes [][]parkedBurl) int {
	n := 0
	for _, f := range flushes {
		n += len(f)
	}
	return n
}

// TestBurlParkBatching: parks coalesce into one time-triggered flush instead
// of a statement per win (handoff 08: per-win INSERTs + a sweep riding every
// win were ~40% of sampled PG time at 150rps).
func TestBurlParkBatching(t *testing.T) {
	b, flushes := testBurlNotifier(t)
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		b.ParkFromWin(ctx, "trace-"+string(rune('a'+i)), "http://dsp/burl")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if countRows(flushes()) == 10 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := flushes()
	if countRows(got) != 10 {
		t.Fatalf("want 10 parked rows flushed, got %d (%d flushes)", countRows(got), len(got))
	}
	// Time-coalesced: 10 near-simultaneous parks must not take 10 statements.
	if len(got) > 2 {
		t.Fatalf("10 parks took %d flushes — batching is not coalescing", len(got))
	}

	// Size trigger: parkBatchMax+10 parks flush the first batch at exactly
	// parkBatchMax rows without waiting out the timer.
	for i := 0; i < parkBatchMax+10; i++ {
		b.ParkFromWin(ctx, "big-"+time.Now().String()+string(rune(i)), "http://dsp/burl")
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if countRows(flushes()) == 10+parkBatchMax+10 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	got = flushes()
	if countRows(got) != 10+parkBatchMax+10 {
		t.Fatalf("want %d total rows, got %d", 10+parkBatchMax+10, countRows(got))
	}
	foundFull := false
	for _, f := range got {
		if len(f) == parkBatchMax {
			foundFull = true
		}
	}
	if !foundFull {
		t.Fatalf("no flush hit parkBatchMax=%d — size trigger broken (flush sizes: %v)", parkBatchMax, flushSizes(got))
	}
}

// TestBurlStopFlushesBuffer: an orderly shutdown drains the buffer — Stop()
// must not strand parks that arrived inside the flush window.
func TestBurlStopFlushesBuffer(t *testing.T) {
	b, flushes := testBurlNotifier(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		b.ParkFromWin(ctx, "stop-"+string(rune('a'+i)), "http://dsp/burl")
	}
	b.Stop() // immediately — before the 25ms timer can fire
	if n := countRows(flushes()); n != 5 {
		t.Fatalf("Stop stranded parks: want 5 flushed, got %d", n)
	}
	// After Stop, parks degrade to direct (synchronous) flush, not silence.
	b.ParkFromWin(ctx, "post-stop", "http://dsp/burl")
	if n := countRows(flushes()); n != 6 {
		t.Fatalf("post-Stop park lost: want 6, got %d", n)
	}
}

func flushSizes(fs [][]parkedBurl) []int {
	out := make([]int, len(fs))
	for i, f := range fs {
		out[i] = len(f)
	}
	return out
}
