package main

import (
	"testing"
	"time"
)

func TestResolvePeriod(t *testing.T) {
	now := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)

	// Default: last calendar month.
	start, end, err := resolvePeriod("", "", "", now)
	if err != nil {
		t.Fatalf("default: %v", err)
	}
	if !start.Equal(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)) || !end.Equal(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("default period = [%v,%v), want [2026-02-01,2026-03-01)", start, end)
	}

	// --month.
	start, end, err = resolvePeriod("2026-06", "", "", now)
	if err != nil {
		t.Fatalf("month: %v", err)
	}
	if !start.Equal(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)) || !end.Equal(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("month period = [%v,%v), want June 2026", start, end)
	}

	// Explicit start/end wins over month.
	start, end, err = resolvePeriod("2026-06", "2026-01-10", "2026-01-20", now)
	if err != nil {
		t.Fatalf("explicit: %v", err)
	}
	if !start.Equal(time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)) || !end.Equal(time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("explicit period = [%v,%v), want [2026-01-10,2026-01-20)", start, end)
	}

	// Bad month string errors.
	if _, _, err := resolvePeriod("nope", "", "", now); err == nil {
		t.Error("expected error for bad month")
	}
}
