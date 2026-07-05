package floors

import "testing"

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
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Effective(tc.base, tc.config, tc.device, tc.geo); got != tc.want {
				t.Errorf("Effective(%v, device=%q, geo=%q) = %v, want %v", tc.base, tc.device, tc.geo, got, tc.want)
			}
		})
	}
}
