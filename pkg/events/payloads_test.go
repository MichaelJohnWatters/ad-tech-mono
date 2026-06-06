package events_test

import (
	"reflect"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

// TestAllEventPayloadsHaveSchemaVersion enforces the
// docs/EVENT_PATHWAY_AUDIT.md schema_version invariant: every wire
// payload in pkg/events and every analytics mirror in
// pkg/store/analytics MUST carry a SchemaVersion field tagged
// `json:"schema_version"` as the FIRST struct field. If you add a
// new event type and this test fails, set the field as the leading
// member of your struct (see e.g. events.AuctionWinEvent).
//
// Go reflection can't enumerate package-level types directly, so the
// payload list below is manually curated. When a new event type
// lands, add it here AND to the struct definition. The two-place
// edit is intentional: it forces a moment of "do I really need a new
// payload type?" rather than letting them proliferate silently.
func TestAllEventPayloadsHaveSchemaVersion(t *testing.T) {
	cases := []struct {
		name string
		typ  reflect.Type
	}{
		// Wire types — pkg/events/payloads.go
		{"events.AuctionWinEvent", reflect.TypeOf(events.AuctionWinEvent{})},
		{"events.AuctionCompleteEvent", reflect.TypeOf(events.AuctionCompleteEvent{})},
		{"events.BudgetDepletedEvent", reflect.TypeOf(events.BudgetDepletedEvent{})},
		{"events.CampaignStateEvent", reflect.TypeOf(events.CampaignStateEvent{})},
		{"events.OptOutEvent", reflect.TypeOf(events.OptOutEvent{})},
		{"events.CacheInvalidateEvent", reflect.TypeOf(events.CacheInvalidateEvent{})},
		{"events.VideoEvent", reflect.TypeOf(events.VideoEvent{})},
		{"events.AudioEvent", reflect.TypeOf(events.AudioEvent{})},
		{"events.AdserverRenderFailedEvent", reflect.TypeOf(events.AdserverRenderFailedEvent{})},
		{"events.AdserverFreqCapBlockedEvent", reflect.TypeOf(events.AdserverFreqCapBlockedEvent{})},
		{"events.TrackerRejectedEvent", reflect.TypeOf(events.TrackerRejectedEvent{})},
		{"events.ServeNoFillEvent", reflect.TypeOf(events.ServeNoFillEvent{})},
		{"events.DirectWinEvent", reflect.TypeOf(events.DirectWinEvent{})},
		{"events.PrebidOutboundWinEvent", reflect.TypeOf(events.PrebidOutboundWinEvent{})},

		// Analytics mirror types — pkg/store/analytics/analytics.go
		// (analytics.Event is a discriminated-union container, not a
		// wire payload; excluded.)
		{"analytics.ImpressionEvent", reflect.TypeOf(analytics.ImpressionEvent{})},
		{"analytics.ClickEvent", reflect.TypeOf(analytics.ClickEvent{})},
		{"analytics.ViewEvent", reflect.TypeOf(analytics.ViewEvent{})},
		{"analytics.ConversionEvent", reflect.TypeOf(analytics.ConversionEvent{})},
		{"analytics.AuctionEvent", reflect.TypeOf(analytics.AuctionEvent{})},
		{"analytics.AuctionWinEvent", reflect.TypeOf(analytics.AuctionWinEvent{})},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.typ.NumField() == 0 {
				t.Fatalf("%s has no fields", tc.name)
			}
			first := tc.typ.Field(0)
			if first.Name != "SchemaVersion" {
				t.Errorf("first field = %q, want SchemaVersion (CLAUDE.md \"field 1\" rule)", first.Name)
			}
			if first.Type.Kind() != reflect.Int {
				t.Errorf("SchemaVersion type = %v, want int", first.Type.Kind())
			}
			if got := first.Tag.Get("json"); got != "schema_version" {
				t.Errorf("SchemaVersion JSON tag = %q, want \"schema_version\"", got)
			}
		})
	}
}
