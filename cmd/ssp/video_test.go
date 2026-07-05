package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildVideoImp(t *testing.T) {
	// Empty/nil config → standard pre-roll defaults.
	def := buildVideoImp(nil)
	if def.Skip != 1 || def.MinDuration != 5 || def.MaxDuration != 30 || def.Plcmt != 1 {
		t.Fatalf("defaults wrong: %+v", def)
	}

	// Non-skippable, 15–30s, plcmt 3, custom mimes/protocols.
	cfg := map[string]any{
		"skippable":    false,
		"min_duration": float64(15),
		"max_duration": float64(30),
		"plcmt":        float64(3),
		"mimes":        []any{"video/mp4"},
		"protocols":    []any{float64(7)},
	}
	v := buildVideoImp(cfg)
	if v.Skip != 0 || v.SkipAfter != 0 || v.SkipMin != 0 {
		t.Errorf("skippable=false should zero skip fields: %+v", v)
	}
	if v.MinDuration != 15 || v.MaxDuration != 30 || v.Plcmt != 3 {
		t.Errorf("duration/plcmt not applied: %+v", v)
	}
	if len(v.Mimes) != 1 || v.Mimes[0] != "video/mp4" || len(v.Protocols) != 1 || v.Protocols[0] != 7 {
		t.Errorf("mimes/protocols not applied: %+v", v)
	}

	// skip_after overrides SkipAfter+SkipMin; unspecified keys keep defaults.
	v2 := buildVideoImp(map[string]any{"skip_after": float64(10)})
	if v2.Skip != 1 || v2.SkipAfter != 10 || v2.SkipMin != 10 || v2.MaxDuration != 30 {
		t.Errorf("skip_after override wrong: %+v", v2)
	}
}

func TestVideoConfigValidateAndJSON(t *testing.T) {
	ip := func(n int) *int { return &n }
	bp := func(b bool) *bool { return &b }

	cases := []struct {
		name    string
		in      *videoConfigInput
		wantErr string
	}{
		{"nil = empty object", nil, ""},
		{"valid", &videoConfigInput{Skippable: bp(false), MinDuration: ip(15), MaxDuration: ip(30), Plcmt: ip(3), Protocols: []int{2, 7}}, ""},
		{"min > max", &videoConfigInput{MinDuration: ip(40), MaxDuration: ip(30)}, "min_duration must be <= max_duration"},
		{"negative duration", &videoConfigInput{MinDuration: ip(-1)}, "must be >= 0"},
		{"bad plcmt", &videoConfigInput{Plcmt: ip(9)}, "plcmt must be 0-4"},
		{"bad linearity", &videoConfigInput{Linearity: ip(5)}, "linearity must be 0-2"},
		{"bad protocol", &videoConfigInput{Protocols: []int{99}}, "protocols must be"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tc.in.validateAndJSON()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if !json.Valid([]byte(out)) {
					t.Fatalf("output not valid JSON: %q", out)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}
