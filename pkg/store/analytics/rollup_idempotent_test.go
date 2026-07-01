package analytics

import (
	"context"
	"testing"
	"time"
)

func TestMemoryStore_InsertRollupsIdempotent(t *testing.T) {
	s := NewMemory()
	ctx := context.Background()
	from := time.Date(2026, 7, 1, 13, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	batch := []RollupRow{
		{Config: "events", Level: "hourly", WindowFrom: from, WindowTo: to, Dimensions: map[string]string{"campaign_id": "c1"}, Metrics: map[string]float64{"count": 60}},
		{Config: "events", Level: "hourly", WindowFrom: from, WindowTo: to, Dimensions: map[string]string{"campaign_id": "c2"}, Metrics: map[string]float64{"count": 40}},
	}

	if err := s.InsertRollups(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if got := s.RollupCount("events", "hourly"); got != 2 {
		t.Fatalf("after first insert = %d, want 2", got)
	}

	// Re-run the SAME window: must replace, not duplicate.
	if err := s.InsertRollups(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if got := s.RollupCount("events", "hourly"); got != 2 {
		t.Fatalf("after re-run = %d, want 2 (idempotent)", got)
	}

	// A different window accumulates.
	next := from.Add(time.Hour)
	if err := s.InsertRollups(ctx, []RollupRow{{Config: "events", Level: "hourly", WindowFrom: next, WindowTo: next.Add(time.Hour), Metrics: map[string]float64{"count": 5}}}); err != nil {
		t.Fatal(err)
	}
	if got := s.RollupCount("events", "hourly"); got != 3 {
		t.Fatalf("after new window = %d, want 3", got)
	}

	// Re-running the first window still replaces only its rows.
	if err := s.InsertRollups(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if got := s.RollupCount("events", "hourly"); got != 3 {
		t.Fatalf("after second re-run = %d, want 3", got)
	}
}
