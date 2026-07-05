package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBidModifiersValidateAndJSON(t *testing.T) {
	cases := []struct {
		name    string
		in      *bidModifiersInput
		wantErr string // substring; "" = success
	}{
		{"nil = empty object", nil, ""},
		{"device + geo ok", &bidModifiersInput{
			Device:     map[string]float64{"mobile": 20},
			GeoCountry: map[string]float64{"USA": 15},
		}, ""},
		{"negative modifier ok (down-bid)", &bidModifiersInput{
			Device: map[string]float64{"desktop": -50},
		}, ""},
		{"too negative", &bidModifiersInput{
			Device: map[string]float64{"desktop": -150},
		}, "out of range"},
		{"too positive", &bidModifiersInput{
			GeoCountry: map[string]float64{"USA": 5000},
		}, "out of range"},
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
