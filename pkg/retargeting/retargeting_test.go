package retargeting

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
)

type fakeSource struct {
	segs map[string][]Segment // accountID → segments
}

func (f *fakeSource) RetargetingSegments(_ context.Context, accountID string) ([]Segment, error) {
	return f.segs[accountID], nil
}

type call struct {
	segment, user string
}

type fakeEnroller struct {
	added       []call
	removed     []call
	invalidated int
	lastTTL     time.Duration
}

func (f *fakeEnroller) AddMembers(_ context.Context, _, segmentID string, users []string, ttl time.Duration) (int, error) {
	f.lastTTL = ttl
	for _, u := range users {
		f.added = append(f.added, call{segmentID, u})
	}
	return len(users), nil // default: all newly added
}

func (f *fakeEnroller) RemoveMember(_ context.Context, _, segmentID, userID string) (int, error) {
	f.removed = append(f.removed, call{segmentID, userID})
	return 1, nil
}

func (f *fakeEnroller) InvalidateAudience(_ context.Context, _ string) error {
	f.invalidated++
	return nil
}

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func visitEvent(account, user, tag string) events.BehaviourSignalEvent {
	return events.BehaviourSignalEvent{Kind: "site_visit", AccountID: account, UserID: user, Tag: tag}
}

func TestOnSiteVisit_EnrollsMatchingSegmentAndInvalidates(t *testing.T) {
	src := &fakeSource{segs: map[string][]Segment{
		"adv": {{ID: "seg-shoes", AccountID: "adv", Type: "retargeting", Rule: []byte(`{"event":"site_visit","tag":"shoes","min_count":1}`)}},
	}}
	enr := &fakeEnroller{}
	s := New(src, enr, testLog())

	enrolled, err := s.OnSiteVisit(context.Background(), visitEvent("adv", "u1", "shoes"))
	if err != nil {
		t.Fatalf("OnSiteVisit: %v", err)
	}
	if len(enrolled) != 1 || enrolled[0] != "seg-shoes" {
		t.Fatalf("enrolled = %v, want [seg-shoes]", enrolled)
	}
	if len(enr.added) != 1 || enr.added[0] != (call{"seg-shoes", "u1"}) {
		t.Errorf("added = %v, want u1→seg-shoes", enr.added)
	}
	if enr.invalidated != 1 {
		t.Errorf("invalidated = %d, want 1 (so the DSP refreshes)", enr.invalidated)
	}
	// TTL comes from the rule's window (none set here → default 30 days) so the
	// member ages out instead of being retargeted forever.
	if enr.lastTTL != 30*24*time.Hour {
		t.Errorf("enroll TTL = %v, want 30d (default window)", enr.lastTTL)
	}
}

// A rule's window_days sets the retargeting TTL.
func TestOnSiteVisit_RuleWindowSetsTTL(t *testing.T) {
	src := &fakeSource{segs: map[string][]Segment{
		"adv": {{ID: "seg", Rule: []byte(`{"event":"site_visit","window_days":7}`)}},
	}}
	enr := &fakeEnroller{}
	s := New(src, enr, testLog())
	if _, err := s.OnSiteVisit(context.Background(), visitEvent("adv", "u1", "")); err != nil {
		t.Fatalf("OnSiteVisit: %v", err)
	}
	if enr.lastTTL != 7*24*time.Hour {
		t.Errorf("enroll TTL = %v, want 7d (rule window)", enr.lastTTL)
	}
}

func TestOnSiteVisit_SkipsTagMismatchAndHighMinCount(t *testing.T) {
	src := &fakeSource{segs: map[string][]Segment{
		"adv": {
			{ID: "seg-tag", Rule: []byte(`{"event":"site_visit","tag":"electronics","min_count":1}`)},   // tag mismatch
			{ID: "seg-freq", Rule: []byte(`{"event":"site_visit","min_count":3}`)},                       // needs history → batch only
			{ID: "seg-other", Rule: []byte(`{"event":"impression","min_count":1}`)},                      // not a visit rule
		},
	}}
	enr := &fakeEnroller{}
	s := New(src, enr, testLog())

	enrolled, err := s.OnSiteVisit(context.Background(), visitEvent("adv", "u1", "shoes"))
	if err != nil {
		t.Fatalf("OnSiteVisit: %v", err)
	}
	if len(enrolled) != 0 {
		t.Errorf("enrolled = %v, want none (tag mismatch / min_count>1 / non-visit)", enrolled)
	}
	if enr.invalidated != 0 {
		t.Errorf("invalidated = %d, want 0 (nothing enrolled)", enr.invalidated)
	}
}

func TestOnSiteVisit_EmptyTagRuleMatchesAnyVisit(t *testing.T) {
	src := &fakeSource{segs: map[string][]Segment{
		"adv": {{ID: "seg-all", Rule: []byte(`{"event":"site_visit"}`)}}, // no tag, default min_count
	}}
	enr := &fakeEnroller{}
	s := New(src, enr, testLog())

	enrolled, _ := s.OnSiteVisit(context.Background(), visitEvent("adv", "u1", "anything"))
	if len(enrolled) != 1 {
		t.Fatalf("empty-tag rule should match any visit, got %v", enrolled)
	}
}

func TestOnSiteVisit_IgnoresNonVisitAndMissingIDs(t *testing.T) {
	enr := &fakeEnroller{}
	s := New(&fakeSource{}, enr, testLog())

	if got, _ := s.OnSiteVisit(context.Background(), events.BehaviourSignalEvent{Kind: "impression", AccountID: "adv", UserID: "u1"}); got != nil {
		t.Errorf("non-visit kind should no-op, got %v", got)
	}
	if got, _ := s.OnSiteVisit(context.Background(), visitEvent("adv", "", "shoes")); got != nil {
		t.Errorf("missing user should no-op, got %v", got)
	}
	if got, _ := s.OnSiteVisit(context.Background(), visitEvent("", "u1", "shoes")); got != nil {
		t.Errorf("missing account should no-op, got %v", got)
	}
	if len(enr.added) != 0 {
		t.Errorf("no enroll calls expected, got %v", enr.added)
	}
}

// Enrolling an already-member (AddMembers reports 0 rows) must not fire an
// invalidate — nothing changed, so the DSP needn't refresh.
func TestOnSiteVisit_AlreadyMemberNoInvalidate(t *testing.T) {
	src := &fakeSource{segs: map[string][]Segment{
		"adv": {{ID: "seg", Rule: []byte(`{"event":"site_visit"}`)}},
	}}
	zero := &zeroAddEnroller{}
	s := New(src, zero, testLog())
	enrolled, _ := s.OnSiteVisit(context.Background(), visitEvent("adv", "u1", ""))
	if len(enrolled) != 0 {
		t.Errorf("already-member should enroll nothing, got %v", enrolled)
	}
	if zero.invalidated != 0 {
		t.Errorf("no invalidate when nothing newly added, got %d", zero.invalidated)
	}
}

type zeroAddEnroller struct{ invalidated int }

func (z *zeroAddEnroller) AddMembers(_ context.Context, _, _ string, _ []string, _ time.Duration) (int, error) {
	return 0, nil
}
func (z *zeroAddEnroller) RemoveMember(_ context.Context, _, _, _ string) (int, error) { return 0, nil }
func (z *zeroAddEnroller) InvalidateAudience(_ context.Context, _ string) error        { z.invalidated++; return nil }

func TestOnConversion_SuppressesFromAllRetargetingSegments(t *testing.T) {
	src := &fakeSource{segs: map[string][]Segment{
		"adv": {{ID: "seg-a"}, {ID: "seg-b"}},
	}}
	enr := &fakeEnroller{}
	s := New(src, enr, testLog())

	removed, err := s.OnConversion(context.Background(), "adv", "u1", "trace-xyz")
	if err != nil {
		t.Fatalf("OnConversion: %v", err)
	}
	if len(removed) != 2 {
		t.Fatalf("removed from %d segments, want 2", len(removed))
	}
	if len(enr.removed) != 2 || enr.invalidated != 1 {
		t.Errorf("removed=%v invalidated=%d, want 2 removes + 1 invalidate", enr.removed, enr.invalidated)
	}
}
