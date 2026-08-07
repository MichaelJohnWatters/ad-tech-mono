package main

import (
	"strings"
	"testing"
)

func TestFreqCapInputValidateAndJSON(t *testing.T) {
	cases := []struct {
		name    string
		in      *freqCapInput
		want    string // exact JSON; "" = expect error
		wantErr string // substring of the error
	}{
		{"nil = no cap", nil, "[]", ""},
		{"zero limit clears", &freqCapInput{Limit: 0}, "[]", ""},
		{"negative limit clears", &freqCapInput{Limit: -3, Scope: "household"}, "[]", ""},
		{"default window + scope omitted", &freqCapInput{Limit: 5},
			`[{"dimension":"line_item","limit":5,"window":"day"}]`, ""},
		{"explicit user scope stored same as default (omitted)", &freqCapInput{Limit: 5, Window: "hour", Scope: "user"},
			`[{"dimension":"line_item","limit":5,"window":"hour"}]`, ""},
		{"household scope rides on the line_item entry", &freqCapInput{Limit: 3, Window: "week", Scope: "household"},
			`[{"dimension":"line_item","limit":3,"scope":"household","window":"week"}]`, ""},
		{"bad window", &freqCapInput{Limit: 5, Window: "fortnight"}, "", "window must be"},
		{"bad scope", &freqCapInput{Limit: 5, Scope: "device"}, "", "scope must be user or household"},
		{"limit too large", &freqCapInput{Limit: 200000}, "", "too large"},
	}
	for _, tc := range cases {
		got, err := tc.in.validateAndJSON()
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: err = %v, want containing %q", tc.name, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: JSON = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// Round trip: what validateAndJSON writes, the edit-meta prefill decoder reads
// back — including the scope, and including pre-scope rows (no scope field →
// "user").
func TestLineItemCapFromJSON(t *testing.T) {
	cases := []struct {
		name, raw  string
		wantLimit  int
		wantWindow string
		wantScope  string
	}{
		{"empty", "", 0, "", ""},
		{"empty array", "[]", 0, "", ""},
		{"malformed", "{nope", 0, "", ""},
		{"pre-scope entry defaults to user", `[{"dimension":"line_item","window":"day","limit":4}]`, 4, "day", "user"},
		{"household scope", `[{"dimension":"line_item","window":"hour","limit":2,"scope":"household"}]`, 2, "hour", "household"},
		{"unknown scope normalises to user", `[{"dimension":"line_item","window":"day","limit":4,"scope":"weird"}]`, 4, "day", "user"},
		{"other dimensions skipped", `[{"dimension":"creative","limit":9},{"dimension":"line_item","window":"week","limit":7}]`, 7, "week", "user"},
	}
	for _, tc := range cases {
		limit, window, scope := lineItemCapFromJSON(tc.raw)
		if limit != tc.wantLimit || window != tc.wantWindow || scope != tc.wantScope {
			t.Errorf("%s: = (%d,%q,%q), want (%d,%q,%q)", tc.name, limit, window, scope, tc.wantLimit, tc.wantWindow, tc.wantScope)
		}
	}
}

// The writer→loader contract: the DSP's stored shape decodes into the exact
// FreqCapScope* values the ad server switches on (via the shared shape both
// decoders use).
func TestFreqCapInputRoundTripScope(t *testing.T) {
	in := &freqCapInput{Limit: 6, Window: "day", Scope: "household"}
	j, err := in.validateAndJSON()
	if err != nil {
		t.Fatal(err)
	}
	limit, window, scope := lineItemCapFromJSON(j)
	if limit != 6 || window != "day" || scope != "household" {
		t.Fatalf("round trip = (%d,%q,%q), want (6,day,household)", limit, window, scope)
	}
}
