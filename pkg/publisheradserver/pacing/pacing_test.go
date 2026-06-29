package pacing

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/publisheradserver"
)

func silentLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func makeLine(committed int64, start, end time.Time, mode string) publisheradserver.PublisherLineItem {
	return publisheradserver.PublisherLineItem{
		ID:                   "li-test",
		ImpressionsCommitted: committed,
		DeliveryStart:        &start,
		DeliveryEnd:          &end,
		PacingMode:           mode,
	}
}

func TestShouldServe(t *testing.T) {
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	midflight := start.Add(12 * time.Hour)

	t.Run("no commitment serves always", func(t *testing.T) {
		l2 := cache.NewMemoryL2()
		tracker := New(l2, silentLog())
		li := publisheradserver.PublisherLineItem{ID: "li-x"} // no commit, no flight
		if !tracker.ShouldServe(li, midflight) {
			t.Fatal("expected serve for no-commitment line item")
		}
	})

	t.Run("asap pacing serves always within flight", func(t *testing.T) {
		l2 := cache.NewMemoryL2()
		tracker := New(l2, silentLog())
		li := makeLine(1000, start, end, publisheradserver.PacingASAP)
		if !tracker.ShouldServe(li, midflight) {
			t.Fatal("expected ASAP to serve regardless of pace")
		}
	})

	t.Run("behind pace → serve", func(t *testing.T) {
		l2 := cache.NewMemoryL2()
		// expected at midflight = 500. actual = 100 → behind.
		_, _ = l2.IncrBy(context.Background(), actualsKey("li-test"), 100)
		tracker := New(l2, silentLog())
		li := makeLine(1000, start, end, publisheradserver.PacingEven)
		if !tracker.ShouldServe(li, midflight) {
			t.Fatal("expected serve when behind pace")
		}
	})

	t.Run("ahead of pace → defer", func(t *testing.T) {
		l2 := cache.NewMemoryL2()
		// expected at midflight = 500. actual = 800 → ahead.
		_, _ = l2.IncrBy(context.Background(), actualsKey("li-test"), 800)
		tracker := New(l2, silentLog())
		li := makeLine(1000, start, end, publisheradserver.PacingEven)
		if tracker.ShouldServe(li, midflight) {
			t.Fatal("expected defer when ahead of pace")
		}
	})

	t.Run("before flight starts → serve", func(t *testing.T) {
		l2 := cache.NewMemoryL2()
		tracker := New(l2, silentLog())
		li := makeLine(1000, start, end, publisheradserver.PacingEven)
		if !tracker.ShouldServe(li, start.Add(-1*time.Hour)) {
			t.Fatal("expected serve before flight (expected<=0 short-circuit)")
		}
	})
}

func TestRecordImpression(t *testing.T) {
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	l2 := cache.NewMemoryL2()
	tracker := New(l2, silentLog())
	li := makeLine(1000, start, end, publisheradserver.PacingEven)

	if err := tracker.RecordImpression(context.Background(), li); err != nil {
		t.Fatalf("RecordImpression: %v", err)
	}
	if err := tracker.RecordImpression(context.Background(), li); err != nil {
		t.Fatalf("RecordImpression: %v", err)
	}

	v, ok, _ := l2.Get(context.Background(), actualsKey(li.ID))
	if !ok {
		t.Fatal("counter not present after RecordImpression")
	}
	if v != "2" {
		t.Errorf("counter: got %q, want 2", v)
	}
}

func TestExpectedByNow(t *testing.T) {
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(100 * time.Hour)
	li := makeLine(1000, start, end, publisheradserver.PacingEven)

	cases := []struct {
		name string
		now  time.Time
		want int64
	}{
		{"before start", start.Add(-1 * time.Hour), 0},
		{"at start", start, 0},
		{"25% in", start.Add(25 * time.Hour), 250},
		{"50% in", start.Add(50 * time.Hour), 500},
		{"75% in", start.Add(75 * time.Hour), 750},
		{"at end", end, 1000},
		{"after end", end.Add(1 * time.Hour), 1000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := expectedByNow(li, tc.now); got != tc.want {
				t.Errorf("expectedByNow: got %d, want %d", got, tc.want)
			}
		})
	}
}
