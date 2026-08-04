package preload

import (
	"encoding/json"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
)

// planFromInvalidate routes an invalidate payload to the cheapest correct refresh:
// user-targeted > segment-scoped > full reconcile.
func TestPlanFromInvalidate(t *testing.T) {
	mustJSON := func(v any) []byte { b, _ := json.Marshal(v); return b }

	cases := []struct {
		name     string
		data     []byte
		wantFull bool
		wantSeg  string
		wantUser []string
	}{
		{"user ids win", mustJSON(events.AudienceInvalidateEvent{UserIDs: []string{"u1", "u2"}, SegmentID: "seg"}), false, "", []string{"u1", "u2"}},
		{"segment only", mustJSON(events.AudienceInvalidateEvent{SegmentID: "seg-A"}), false, "seg-A", nil},
		{"empty payload → full", mustJSON(events.AudienceInvalidateEvent{Source: "x"}), true, "", nil},
		{"legacy source-only json → full", []byte(`{"source":"audience-rt"}`), true, "", nil},
		{"garbled → full", []byte(`not json`), true, "", nil},
		{"legacy segment json still delta", []byte(`{"segment_id":"seg-B","account_id":"a"}`), false, "seg-B", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := planFromInvalidate(tc.data)
			if p.full != tc.wantFull {
				t.Errorf("full=%v want %v", p.full, tc.wantFull)
			}
			if p.segment != tc.wantSeg {
				t.Errorf("segment=%q want %q", p.segment, tc.wantSeg)
			}
			if len(p.userIDs) != len(tc.wantUser) {
				t.Errorf("userIDs=%v want %v", p.userIDs, tc.wantUser)
			}
		})
	}
}
