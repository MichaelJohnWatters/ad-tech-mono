package floors

import (
	"testing"
	"time"
)

func TestEffective(t *testing.T) {
	cfg := map[string]any{
		"device": map[string]any{"mobile": 1.5, "desktop": 2.0},
		"geo":    map[string]any{"USA": 2.5, "GBR": 1.8},
	}
	cases := []struct {
		name           string
		base           float64
		device, geo    string
		config         map[string]any
		want           float64
	}{
		{"nil config = base", 1.0, "mobile", "USA", nil, 1.0},
		{"no match = base", 1.0, "ctv", "FRA", cfg, 1.0},
		{"device override", 1.0, "mobile", "FRA", cfg, 1.5},
		{"geo override", 1.0, "ctv", "USA", cfg, 2.5},
		{"device+geo takes max", 1.0, "mobile", "USA", cfg, 2.5},
		{"device+geo takes max (device higher)", 1.0, "desktop", "GBR", cfg, 2.0},
		{"base higher than overrides wins", 3.0, "mobile", "USA", cfg, 3.0},
		{"case-insensitive device", 1.0, "MOBILE", "FRA", cfg, 1.5},
		{"case-insensitive geo", 1.0, "ctv", "usa", cfg, 2.5},
		{"empty device/geo = base", 1.0, "", "", cfg, 1.0},
	}
	// device/geo cases are time-agnostic; a fixed reference time keeps them so.
	ref := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Effective(tc.base, tc.config, tc.device, tc.geo, ref); got != tc.want {
				t.Errorf("Effective(%v, device=%q, geo=%q) = %v, want %v", tc.base, tc.device, tc.geo, got, tc.want)
			}
		})
	}
}

func TestEffectiveDayparts(t *testing.T) {
	// Weekday premium 09:00–18:00 UTC = 3.0; overnight wrap 22:00–04:00 = 4.0.
	cfg := map[string]any{
		"device": map[string]any{"mobile": 1.5},
		"dayparts": []any{
			map[string]any{"days": []any{1.0, 2.0, 3.0, 4.0, 5.0}, "start_hour": 9.0, "end_hour": 18.0, "floor": 3.0},
			map[string]any{"start_hour": 22.0, "end_hour": 4.0, "floor": 4.0},
		},
	}
	// 2026-07-02 is a Thursday (weekday 4); 2026-07-04 is Saturday (6).
	thu := func(h int) time.Time { return time.Date(2026, 7, 2, h, 30, 0, 0, time.UTC) }
	sat := func(h int) time.Time { return time.Date(2026, 7, 4, h, 30, 0, 0, time.UTC) }

	cases := []struct {
		name        string
		base        float64
		device      string
		at          time.Time
		want        float64
	}{
		{"weekday inside window", 1.0, "ctv", thu(10), 3.0},
		{"weekday before window", 1.0, "ctv", thu(8), 1.0},
		{"weekday at end is exclusive", 1.0, "ctv", thu(18), 1.0},
		{"weekend excluded from weekday window", 1.0, "ctv", sat(10), 1.0},
		{"overnight wrap late night", 1.0, "ctv", thu(23), 4.0},
		{"overnight wrap early morning", 1.0, "ctv", sat(2), 4.0},
		{"overnight wrap boundary end exclusive", 1.0, "ctv", sat(4), 1.0},
		{"daypart beats device", 1.0, "mobile", thu(10), 3.0},
		{"device beats non-matching daypart", 1.0, "mobile", thu(6), 1.5},
		{"base beats all", 5.0, "mobile", thu(10), 5.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Effective(tc.base, cfg, tc.device, "", tc.at); got != tc.want {
				t.Errorf("Effective(base=%v, device=%q, at=%v) = %v, want %v", tc.base, tc.device, tc.at, got, tc.want)
			}
		})
	}
}

func TestEffectiveDaypartTimezone(t *testing.T) {
	// 15:00 UTC = 10:00 America/New_York (EDT, UTC-5 in July). A 09:00–17:00
	// NY window must match on NY-local hours, not UTC hours.
	cfg := map[string]any{
		"timezone": "America/New_York",
		"dayparts": []any{
			map[string]any{"start_hour": 9.0, "end_hour": 17.0, "floor": 2.5},
		},
	}
	inNY := time.Date(2026, 7, 2, 15, 0, 0, 0, time.UTC) // 11:00 NY → inside
	outNY := time.Date(2026, 7, 2, 23, 0, 0, 0, time.UTC) // 19:00 NY → outside
	if got := Effective(1.0, cfg, "ctv", "", inNY); got != 2.5 {
		t.Errorf("inside NY window = %v, want 2.5", got)
	}
	if got := Effective(1.0, cfg, "ctv", "", outNY); got != 1.0 {
		t.Errorf("outside NY window = %v, want 1.0", got)
	}
}
