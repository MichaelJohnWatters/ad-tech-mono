package cache

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestSelfHealingL2ImmediateConnect(t *testing.T) {
	real := NewMemoryL2()
	s := NewSelfHealingL2(func(context.Context) (L2Cache, error) {
		return real, nil
	}, time.Minute, "test", discardLog())

	ctx := context.Background()
	if _, err := s.Incr(ctx, "k"); err != nil {
		t.Fatalf("incr: %v", err)
	}
	if n, _ := real.Incr(ctx, "k"); n != 2 {
		t.Fatalf("write did not land on the real backend: counter = %d, want 2", n)
	}
}

func TestSelfHealingL2FallsBackThenHeals(t *testing.T) {
	real := NewMemoryL2()
	var attempts atomic.Int64
	dial := func(context.Context) (L2Cache, error) {
		if attempts.Add(1) < 3 {
			return nil, errors.New("connection refused")
		}
		return real, nil
	}

	s := NewSelfHealingL2(dial, 10*time.Millisecond, "test", discardLog())
	ctx := context.Background()

	// Fallback era: writes land in memory, requests never error.
	if _, err := s.Incr(ctx, "k"); err != nil {
		t.Fatalf("fallback incr: %v", err)
	}

	// Wait for the background loop to land the real client — detected by
	// behaviour (writes reaching `real`), since the fallback is also a
	// MemoryL2 and a type check can't tell them apart.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := s.Incr(ctx, "probe"); err != nil {
			t.Fatalf("probe incr: %v", err)
		}
		if _, found, _ := real.Get(ctx, "probe"); found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never healed after %d dial attempts", attempts.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSelfHealingL2CloseStopsRetry(t *testing.T) {
	var attempts atomic.Int64
	dial := func(context.Context) (L2Cache, error) {
		attempts.Add(1)
		return nil, errors.New("connection refused")
	}
	s := NewSelfHealingL2(dial, 5*time.Millisecond, "test", discardLog())
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	settled := attempts.Load()
	time.Sleep(30 * time.Millisecond)
	if got := attempts.Load(); got > settled+1 {
		t.Fatalf("retry loop kept dialing after Close: %d -> %d", settled, got)
	}
}
