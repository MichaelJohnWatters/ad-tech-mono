package privacy

import "testing"

func TestAllowsResidency(t *testing.T) {
	for _, tc := range []struct {
		req, home string
		want      bool
	}{
		{"", "us-east-1", true},          // no assertion → allowed
		{"us-east-1", "", true},          // no home region → gate off
		{"us-east-1", "us-east-1", true}, // in-region
		{"eu", "us-east-1", false},       // out-of-region → suppress
		{"eu", "eu", true},               // in-region (eu deploy)
		{"US-East-1", "us-east-1", true}, // case-insensitive: no lockout on case drift
		{" us-east-1 ", "us-east-1", true},
		{"eu", "US-EAST-1", false}, // still out-of-region after normalising
	} {
		if got := AllowsResidency(tc.req, tc.home); got != tc.want {
			t.Errorf("AllowsResidency(%q,%q)=%v want %v", tc.req, tc.home, got, tc.want)
		}
	}
}

func TestAllowsUserData(t *testing.T) {
	yes := Decision{Personalise: true}
	no := Decision{Personalise: false}
	if AllowsUserData(no, "us-east-1", "us-east-1") {
		t.Error("no-personalise must block user data regardless of region")
	}
	if !AllowsUserData(yes, "us-east-1", "us-east-1") {
		t.Error("consented + in-region must allow")
	}
	if AllowsUserData(yes, "eu", "us-east-1") {
		t.Error("consented but out-of-region must block")
	}
	if !AllowsUserData(yes, "", "us-east-1") {
		t.Error("consented + no residency assertion must allow")
	}
}
