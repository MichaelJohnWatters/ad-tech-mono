package analytics

import (
	"context"
	"testing"
	"time"
)

// TestViewableImpressionsForUsers covers the view-through lookback the memory
// backend serves (parity with the ClickHouse SQL): scope by user-id set +
// account (+ optional campaign) since a window, viewability recovered by
// correlating the views slice, most-recent first, and the requireViewable gate.
func TestViewableImpressionsForUsers(t *testing.T) {
	ctx := context.Background()
	s := NewMemory()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)

	// Two impressions for user u1 on advertiser acct-A campaign camp-1: an older
	// viewable one and a newer NON-viewable one. Plus noise: another user, another
	// account, and one outside the window.
	imps := []*BehaviourSignalRow{
		{Kind: "impression", TraceID: "t-view-old", UserID: "u1", AccountID: "acct-A", CampaignID: "camp-1", ObservedAt: now.Add(-48 * time.Hour)},
		{Kind: "impression", TraceID: "t-noview-new", UserID: "u1", AccountID: "acct-A", CampaignID: "camp-1", ObservedAt: now.Add(-2 * time.Hour)},
		{Kind: "impression", TraceID: "t-other-user", UserID: "u2", AccountID: "acct-A", CampaignID: "camp-1", ObservedAt: now.Add(-1 * time.Hour)},
		{Kind: "impression", TraceID: "t-other-acct", UserID: "u1", AccountID: "acct-B", CampaignID: "camp-1", ObservedAt: now.Add(-1 * time.Hour)},
		{Kind: "impression", TraceID: "t-too-old", UserID: "u1", AccountID: "acct-A", CampaignID: "camp-1", ObservedAt: now.Add(-400 * time.Hour)},
		// A click row must never surface as a view-through exposure.
		{Kind: "click", TraceID: "t-click", UserID: "u1", AccountID: "acct-A", CampaignID: "camp-1", ObservedAt: now.Add(-3 * time.Hour)},
	}
	if err := s.InsertBehaviourSignals(ctx, imps); err != nil {
		t.Fatal(err)
	}
	// Only t-view-old is IAB-viewable.
	if err := s.InsertView(ctx, &ViewEvent{TraceID: "t-view-old", IABViewable: true, Timestamp: now.Add(-48 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertView(ctx, &ViewEvent{TraceID: "t-noview-new", IABViewable: false, Timestamp: now.Add(-2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	since := now.Add(-168 * time.Hour) // 7-day view window

	// requireViewable=true → only the viewable exposure, and it's the last touch.
	got, err := s.ViewableImpressionsForUsers(ctx, []string{"u1"}, "acct-A", "camp-1", since, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].TraceID != "t-view-old" || !got[0].Viewable {
		t.Fatalf("requireViewable: got %+v, want just t-view-old (viewable)", got)
	}

	// requireViewable=false → both u1 exposures in-window, most-recent first.
	got, err = s.ViewableImpressionsForUsers(ctx, []string{"u1"}, "acct-A", "camp-1", since, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].TraceID != "t-noview-new" || got[1].TraceID != "t-view-old" {
		t.Fatalf("no-require: got %+v, want [t-noview-new, t-view-old] (most-recent first)", got)
	}
	if got[0].Viewable {
		t.Errorf("t-noview-new should carry Viewable=false")
	}

	// Account scope with empty campaign still finds acct-A exposures, excludes acct-B.
	got, err = s.ViewableImpressionsForUsers(ctx, []string{"u1"}, "acct-A", "", since, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range got {
		if r.TraceID == "t-other-acct" {
			t.Errorf("acct-B exposure leaked into acct-A scope")
		}
	}

	// Cross-device: resolving u1→{u1,u2} surfaces u2's exposure too.
	got, err = s.ViewableImpressionsForUsers(ctx, []string{"u1", "u2"}, "acct-A", "camp-1", since, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("cross-device set: got %d exposures, want 3 (u1 x2 + u2 x1)", len(got))
	}
}
