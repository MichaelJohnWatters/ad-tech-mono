package openrtb

import "testing"

func TestHouseholdEIDRoundTrip(t *testing.T) {
	u := &User{ID: "user-1", EIDs: []EID{UID2EID("tok"), HouseholdEID("hh:abc123")}}
	if got := HouseholdFrom(u); got != "hh:abc123" {
		t.Errorf("HouseholdFrom = %q, want hh:abc123", got)
	}
	// UID2 extraction is unaffected by the extra EID.
	if got := UID2From(u); got != "tok" {
		t.Errorf("UID2From = %q, want tok", got)
	}
}

func TestHouseholdFromAbsent(t *testing.T) {
	if got := HouseholdFrom(nil); got != "" {
		t.Errorf("nil user: %q, want empty", got)
	}
	if got := HouseholdFrom(&User{ID: "u"}); got != "" {
		t.Errorf("no EIDs: %q, want empty", got)
	}
	if got := HouseholdFrom(&User{EIDs: []EID{UID2EID("tok")}}); got != "" {
		t.Errorf("uid2 only: %q, want empty", got)
	}
}

func TestHouseholdEIDShape(t *testing.T) {
	e := HouseholdEID("hh:x")
	if e.Source != HouseholdSource {
		t.Errorf("source = %q", e.Source)
	}
	if len(e.UIDs) != 1 || e.UIDs[0].AType != HouseholdAType {
		t.Errorf("uids = %+v", e.UIDs)
	}
}
