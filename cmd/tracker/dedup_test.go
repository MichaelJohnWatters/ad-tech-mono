package main

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache"
)

func TestDedup_FirstSeen(t *testing.T) {
	d := NewDedup(cache.NewMemoryL2(), time.Hour, true, slog.New(slog.NewTextHandler(nopWriter{}, nil)))
	ctx := context.Background()

	if !d.FirstSeen(ctx, "impression", "trace-1") {
		t.Fatal("first request should be FirstSeen")
	}
	if d.FirstSeen(ctx, "impression", "trace-1") {
		t.Fatal("second request for same trace should be a duplicate")
	}
}

func TestDedup_DistinctEventTypes(t *testing.T) {
	d := NewDedup(cache.NewMemoryL2(), time.Hour, true, slog.New(slog.NewTextHandler(nopWriter{}, nil)))
	ctx := context.Background()

	if !d.FirstSeen(ctx, "impression", "trace-1") {
		t.Fatal("impression should be FirstSeen")
	}
	if !d.FirstSeen(ctx, "click", "trace-1") {
		t.Fatal("click on same trace must be independent of impression")
	}
}

func TestDedup_Disabled(t *testing.T) {
	d := NewDedup(cache.NewMemoryL2(), time.Hour, false, slog.New(slog.NewTextHandler(nopWriter{}, nil)))
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if !d.FirstSeen(ctx, "impression", "trace-1") {
			t.Fatal("disabled dedup must always return true")
		}
	}
}

func TestDedup_EmptyTraceBypass(t *testing.T) {
	d := NewDedup(cache.NewMemoryL2(), time.Hour, true, slog.New(slog.NewTextHandler(nopWriter{}, nil)))
	ctx := context.Background()
	if !d.FirstSeen(ctx, "impression", "") {
		t.Fatal("empty trace id should always return true (cannot dedup without an id)")
	}
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
