package invoicing

import (
	"testing"
	"time"
)

// microsPerDollar must convert exactly with no cents rounding: the platform is
// micros-native and invoices.total is DECIMAL dollars.
func TestMicrosToDollars(t *testing.T) {
	cases := []struct {
		micros int64
		want   float64
	}{
		{1_000_000, 1.00},
		{1_500_000, 1.50},
		{500_000, 0.50},
		{0, 0.00},
		{2_345_678, 2.345678},
	}
	for _, c := range cases {
		got := float64(c.micros) / microsPerDollar
		if got != c.want {
			t.Errorf("%d micros → %v, want %v", c.micros, got, c.want)
		}
	}
}

func TestLastCalendarMonth(t *testing.T) {
	// Mid-March 2026 → February 2026 [Feb 1, Mar 1).
	start, end := LastCalendarMonth(time.Date(2026, 3, 15, 9, 0, 0, 0, time.UTC))
	if !start.Equal(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("start = %v, want 2026-02-01", start)
	}
	if !end.Equal(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("end = %v, want 2026-03-01", end)
	}

	// January rolls back to the previous December.
	start, end = LastCalendarMonth(time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC))
	if !start.Equal(time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("jan start = %v, want 2025-12-01", start)
	}
	if !end.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("jan end = %v, want 2026-01-01", end)
	}
}
