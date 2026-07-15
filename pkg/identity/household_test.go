package identity

import (
	"strings"
	"testing"
)

func TestHouseholdIDDeterministic(t *testing.T) {
	a := HouseholdID("salt", "203.0.113.7")
	b := HouseholdID("salt", "203.0.113.7")
	if a == "" || a != b {
		t.Errorf("same salt+ip must derive the same id: %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, HouseholdIDPrefix) {
		t.Errorf("id %q missing %q prefix", a, HouseholdIDPrefix)
	}
	if len(a) != len(HouseholdIDPrefix)+16 {
		t.Errorf("id %q length %d, want prefix+16 hex", a, len(a))
	}
}

func TestHouseholdIDVariesByInputs(t *testing.T) {
	base := HouseholdID("salt", "203.0.113.7")
	if HouseholdID("salt", "203.0.113.8") == base {
		t.Error("different IPs must derive different households")
	}
	if HouseholdID("other-salt", "203.0.113.7") == base {
		t.Error("different salts must derive different households (rotation invalidates ids)")
	}
}

func TestHouseholdIDEmptyIP(t *testing.T) {
	if got := HouseholdID("salt", ""); got != "" {
		t.Errorf("empty ip: %q, want empty (no household signal)", got)
	}
}
