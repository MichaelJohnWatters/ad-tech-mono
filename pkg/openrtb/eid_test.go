package openrtb

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestUID2EID(t *testing.T) {
	e := UID2EID("tok-123")
	if e.Source != UID2Source {
		t.Errorf("source = %q, want %q", e.Source, UID2Source)
	}
	if len(e.UIDs) != 1 || e.UIDs[0].ID != "tok-123" {
		t.Fatalf("uids = %+v, want one uid tok-123", e.UIDs)
	}
	if e.UIDs[0].AType != AgentTypePersonPersist {
		t.Errorf("atype = %d, want %d (person-persistent)", e.UIDs[0].AType, AgentTypePersonPersist)
	}
}

func TestEIDMarshalShape(t *testing.T) {
	u := &User{ID: "", EIDs: []EID{UID2EID("abc")}}
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, `"eids"`) || !strings.Contains(s, `"source":"uidapi.com"`) {
		t.Errorf("unexpected EID JSON: %s", s)
	}
	// Round-trips back.
	var back User
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if UID2From(&back) != "abc" {
		t.Errorf("round-trip UID2 = %q, want abc", UID2From(&back))
	}
}

func TestUID2From(t *testing.T) {
	if got := UID2From(nil); got != "" {
		t.Errorf("nil user: got %q, want empty", got)
	}
	if got := UID2From(&User{ID: "x"}); got != "" {
		t.Errorf("no eids: got %q, want empty", got)
	}
	// Ignores non-UID2 sources, picks the UID2 one.
	u := &User{EIDs: []EID{
		{Source: "liveramp.com", UIDs: []UID{{ID: "ramp-1"}}},
		UID2EID("uid2-1"),
	}}
	if got := UID2From(u); got != "uid2-1" {
		t.Errorf("got %q, want uid2-1", got)
	}
}

func TestUserKey(t *testing.T) {
	tests := []struct {
		name string
		user *User
		want string
	}{
		{"nil", nil, ""},
		{"platform id wins", &User{ID: "pid", EIDs: []EID{UID2EID("uid2")}}, "pid"},
		{"falls back to uid2 when no id", &User{EIDs: []EID{UID2EID("uid2")}}, "uid2"},
		{"anonymous", &User{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := UserKey(tt.user); got != tt.want {
				t.Errorf("UserKey = %q, want %q", got, tt.want)
			}
		})
	}
}
