package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFloorConfigValidateAndJSON(t *testing.T) {
	cases := []struct {
		name    string
		in      *floorConfigInput
		wantErr string // substring; "" = expect success
	}{
		{"nil = empty object", nil, ""},
		{"device+geo ok", &floorConfigInput{
			Device: map[string]float64{"mobile": 1.5},
			Geo:    map[string]float64{"USA": 2.0},
		}, ""},
		{"negative device floor", &floorConfigInput{
			Device: map[string]float64{"mobile": -1},
		}, "must be >= 0"},
		{"valid daypart", &floorConfigInput{
			Timezone: "America/New_York",
			Dayparts: []daypartInput{{Days: []int{1, 2, 3, 4, 5}, StartHour: 9, EndHour: 18, Floor: 3.0}},
		}, ""},
		{"overnight wrap daypart ok", &floorConfigInput{
			Dayparts: []daypartInput{{StartHour: 22, EndHour: 4, Floor: 4.0}},
		}, ""},
		{"bad timezone", &floorConfigInput{
			Timezone: "Mars/Olympus",
		}, "invalid timezone"},
		{"negative daypart floor", &floorConfigInput{
			Dayparts: []daypartInput{{StartHour: 9, EndHour: 18, Floor: -2}},
		}, "floor must be >= 0"},
		{"start hour out of range", &floorConfigInput{
			Dayparts: []daypartInput{{StartHour: 24, EndHour: 18, Floor: 1}},
		}, "hours must be"},
		{"end hour out of range", &floorConfigInput{
			Dayparts: []daypartInput{{StartHour: 9, EndHour: 25, Floor: 1}},
		}, "hours must be"},
		{"day out of range", &floorConfigInput{
			Dayparts: []daypartInput{{Days: []int{7}, StartHour: 9, EndHour: 18, Floor: 1}},
		}, "must be 0-6"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tc.in.validateAndJSON()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if !json.Valid([]byte(out)) {
					t.Fatalf("output is not valid JSON: %q", out)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}
