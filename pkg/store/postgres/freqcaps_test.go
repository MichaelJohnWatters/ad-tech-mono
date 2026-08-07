package postgres

import (
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
)

// parseLineItemCap feeds the ad server's freq-cap warm cache — the decode must
// tolerate the pre-scope entry shape (no "scope" field → per-user default) and
// pick up the household scope the DSP now writes on the line_item entry.
func TestParseLineItemCap(t *testing.T) {
	cases := []struct {
		name, raw  string
		wantLimit  int
		wantWindow time.Duration
		wantScope  string
		wantOK     bool
	}{
		{"empty", "", 0, 0, "", false},
		{"empty array", "[]", 0, 0, "", false},
		{"malformed json", "{", 0, 0, "", false},
		{"no line_item entry", `[{"dimension":"creative","window":"day","limit":3}]`, 0, 0, "", false},
		{"zero limit ignored", `[{"dimension":"line_item","window":"day","limit":0}]`, 0, 0, "", false},
		{"pre-scope entry defaults to user scope",
			`[{"dimension":"line_item","window":"hour","limit":5}]`,
			5, time.Hour, models.FreqCapScopeUser, true},
		{"household scope on the line_item entry",
			`[{"dimension":"line_item","window":"week","limit":2,"scope":"household"}]`,
			2, 7 * 24 * time.Hour, models.FreqCapScopeHousehold, true},
		{"unknown scope normalises to user",
			`[{"dimension":"line_item","window":"day","limit":4,"scope":"pet"}]`,
			4, 24 * time.Hour, models.FreqCapScopeUser, true},
		{"unknown window defaults to a day",
			`[{"dimension":"line_item","window":"month","limit":4}]`,
			4, 24 * time.Hour, models.FreqCapScopeUser, true},
	}
	for _, tc := range cases {
		limit, window, scope, ok := parseLineItemCap(tc.raw)
		if limit != tc.wantLimit || window != tc.wantWindow || scope != tc.wantScope || ok != tc.wantOK {
			t.Errorf("%s: parseLineItemCap = (%d,%v,%q,%v), want (%d,%v,%q,%v)",
				tc.name, limit, window, scope, ok, tc.wantLimit, tc.wantWindow, tc.wantScope, tc.wantOK)
		}
	}
}
