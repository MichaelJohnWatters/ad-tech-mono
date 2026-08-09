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

func (f *fakeEnroller) AddMembers(_ context.Context, _, segmentID string, users []string, ttl time.Duration, _, _ string) (int, error) {
	f.lastTTL = ttl
	for _, u := range users {
		f.added = append(f.added, call{segmentID, u})
	}
	return len(users), nil // default: all newly added
}

func (f *fakeEnroller) RemoveMember(_ context.Context, _, segmentID, userID, _ string) (int, error) {
	f.removed = append(f.removed, call{segmentID, userID})
	return 1, nil
}

func (f *fakeEnroller) InvalidateAudience(_ context.Context, _ string) error {
	f.invalidated++
	return nil
}

// fakeProductViews records the SKU writes/removes the service makes and tracks
// a per-user carted-SKU set so CountProductViews reflects record/remove.
type fakeProductViews struct {
	recorded map[string][]string // userID → skus recorded (append log)
	views    map[string][]string // userID → current carted skus (for Count)
	lastTTL  time.Duration
	removed  []call // {sku, user}
}

func (f *fakeProductViews) RecordProductViews(_ context.Context, _, userID string, skus []string, _ time.Time, ttl time.Duration, _ string) error {
	if f.recorded == nil {
		f.recorded = map[string][]string{}
		f.views = map[string][]string{}
	}
	f.recorded[userID] = append(f.recorded[userID], skus...)
	f.views[userID] = append(f.views[userID], skus...)
	f.lastTTL = ttl
	return nil
}

func (f *fakeProductViews) RemoveProductViews(_ context.Context, _, userID string, skus []string) error {
	drop := map[string]bool{}
	for _, s := range skus {
		drop[s] = true
		f.removed = append(f.removed, call{s, userID})
	}
	if f.views != nil {
		kept := f.views[userID][:0]
		for _, s := range f.views[userID] {
			if !drop[s] {
				kept = append(kept, s)
			}
		}
		f.views[userID] = kept
	}
	return nil
}

func (f *fakeProductViews) CountProductViews(_ context.Context, _, userID string) (int, error) {
	if f.views == nil {
		return 0, nil
	}
	return len(f.views[userID]), nil
}

// fakeSuppression records whole-person + per-SKU burns.
type fakeSuppression struct {
	personBurns []string        // user ids whole-person burned
	skuBurns    map[string]bool // "user|sku"
}

func (f *fakeSuppression) Suppress(_ context.Context, _ string, userIDs []string, _ time.Time, _ time.Duration, _ string) error {
	f.personBurns = append(f.personBurns, userIDs...)
	return nil
}
func (f *fakeSuppression) SuppressedAt(_ context.Context, _, _ string) (time.Time, bool, error) {
	return time.Time{}, false, nil
}
func (f *fakeSuppression) ClearSuppression(_ context.Context, _, _ string) error { return nil }
func (f *fakeSuppression) SuppressSKUs(_ context.Context, _ string, userIDs, skus []string, _ time.Time, _ time.Duration, _ string) error {
	if f.skuBurns == nil {
		f.skuBurns = map[string]bool{}
	}
	for _, u := range userIDs {
		for _, s := range skus {
			f.skuBurns[u+"|"+s] = true
		}
	}
	return nil
}

// fakeComplements returns a fixed complement map.
type fakeComplements struct{ m map[string][]string }

func (f fakeComplements) ComplementSKUs(_ context.Context, _ string, skus []string) ([]string, error) {
	var out []string
	for _, s := range skus {
		out = append(out, f.m[s]...)
	}
	return out, nil
}

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func visitEvent(account, user, tag string) events.BehaviourSignalEvent {
	return events.BehaviourSignalEvent{Kind: "site_visit", AccountID: account, UserID: user, Tag: tag}
}

// A site_visit carrying SKUs records them per-user with the product-view TTL,
// independent of segment matching (a product-page pixel builds SKU memory even
// before the visitor enrolls).
func TestOnSiteVisit_RecordsProductSKUs(t *testing.T) {
	src := &fakeSource{segs: map[string][]Segment{}} // no segments → no enroll
	enr := &fakeEnroller{}
	pv := &fakeProductViews{}
	s := New(src, enr, testLog())
	s.SetProductViews(pv, func() time.Duration { return 14 * 24 * time.Hour })

	ev := visitEvent("adv", "u1", "product")
	ev.SKUs = "DOG-KIBBLE-12KG, DOG-TREAT-BOX ,DOG-KIBBLE-12KG" // dupes + spaces
	if _, err := s.OnSiteVisit(context.Background(), ev); err != nil {
		t.Fatalf("OnSiteVisit: %v", err)
	}
	got := pv.recorded["u1"]
	if len(got) != 2 || got[0] != "DOG-KIBBLE-12KG" || got[1] != "DOG-TREAT-BOX" {
		t.Fatalf("recorded SKUs = %v, want [DOG-KIBBLE-12KG DOG-TREAT-BOX] (deduped, trimmed)", got)
	}
	if pv.lastTTL != 14*24*time.Hour {
		t.Errorf("product-view TTL = %v, want 14d", pv.lastTTL)
	}
}

// Household enrollment being on records the SKUs for the household id too.
func TestOnSiteVisit_ProductSKUsHouseholdEnroll(t *testing.T) {
	src := &fakeSource{segs: map[string][]Segment{}}
	pv := &fakeProductViews{}
	s := New(src, &fakeEnroller{}, testLog())
	s.SetProductViews(pv, func() time.Duration { return 30 * 24 * time.Hour })
	s.SetHouseholdEnroll(func() bool { return true })

	ev := visitEvent("adv", "u1", "product")
	ev.SKUs = "SKU-A"
	ev.HouseholdID = "hh:home1"
	if _, err := s.OnSiteVisit(context.Background(), ev); err != nil {
		t.Fatalf("OnSiteVisit: %v", err)
	}
	if len(pv.recorded["u1"]) != 1 || len(pv.recorded["hh:home1"]) != 1 {
		t.Fatalf("recorded = %v, want both u1 and hh:home1", pv.recorded)
	}
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
			{ID: "seg-tag", Rule: []byte(`{"event":"site_visit","tag":"electronics","min_count":1}`)}, // tag mismatch
			{ID: "seg-freq", Rule: []byte(`{"event":"site_visit","min_count":3}`)},                    // needs history → batch only
			{ID: "seg-other", Rule: []byte(`{"event":"impression","min_count":1}`)},                   // not a visit rule
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

func (z *zeroAddEnroller) AddMembers(_ context.Context, _, _ string, _ []string, _ time.Duration, _, _ string) (int, error) {
	return 0, nil
}
func (z *zeroAddEnroller) RemoveMember(_ context.Context, _, _, _, _ string) (int, error) {
	return 0, nil
}
func (z *zeroAddEnroller) InvalidateAudience(_ context.Context, _ string) error {
	z.invalidated++
	return nil
}

func TestOnConversion_SuppressesFromAllRetargetingSegments(t *testing.T) {
	src := &fakeSource{segs: map[string][]Segment{
		"adv": {{ID: "seg-a"}, {ID: "seg-b"}},
	}}
	enr := &fakeEnroller{}
	s := New(src, enr, testLog())

	// Generic conversion (no bought SKUs) → whole-person suppression.
	removed, err := s.OnConversion(context.Background(), "adv", "u1", "trace-xyz", nil)
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

// A SKU purchase with items still in the cart is PER-PRODUCT: burn + remove the
// bought SKU, cross-sell its complement, and keep the buyer in the chase (no
// whole-person segment removal).
func TestOnConversion_PerProductKeepsChasingAndCrossSells(t *testing.T) {
	src := &fakeSource{segs: map[string][]Segment{"adv": {{ID: "seg-a"}}}}
	enr := &fakeEnroller{}
	pv := &fakeProductViews{}
	sup := &fakeSuppression{}
	s := New(src, enr, testLog())
	s.SetProductViews(pv, func() time.Duration { return 30 * 24 * time.Hour })
	s.SetSuppression(sup, nil, func() time.Duration { return 30 * 24 * time.Hour })
	s.SetComplements(fakeComplements{m: map[string][]string{"SKU-KIBBLE": {"SKU-TREATS"}}})

	// The user still has another carted item (SKU-BOWL) beyond what they bought.
	_ = pv.RecordProductViews(context.Background(), "adv", "u1", []string{"SKU-KIBBLE", "SKU-BOWL"}, time.Time{}, 0, "")

	removed, err := s.OnConversion(context.Background(), "adv", "u1", "trace-1", []string{"SKU-KIBBLE"})
	if err != nil {
		t.Fatalf("OnConversion: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("per-product purchase whole-person suppressed (removed=%v); should keep chasing", removed)
	}
	if len(enr.removed) != 0 {
		t.Errorf("segment membership removed on a per-product purchase: %v", enr.removed)
	}
	if !sup.skuBurns["u1|SKU-KIBBLE"] {
		t.Errorf("bought SKU-KIBBLE not per-product burned")
	}
	if len(sup.personBurns) != 0 {
		t.Errorf("whole-person burn written on a keep-chasing purchase: %v", sup.personBurns)
	}
	// Bought SKU removed from views; complement recorded.
	got := pv.recorded["u1"]
	if len(got) < 3 || got[len(got)-1] != "SKU-TREATS" {
		t.Errorf("cross-sell complement not recorded: %v", got)
	}
	foundRemove := false
	for _, c := range pv.removed {
		if c.segment == "SKU-KIBBLE" && c.user == "u1" {
			foundRemove = true
		}
	}
	if !foundRemove {
		t.Errorf("bought SKU not removed from views: %v", pv.removed)
	}
	if enr.invalidated == 0 {
		t.Errorf("audience not invalidated after per-product suppress (DSP won't refresh the creative)")
	}
}

// A SKU purchase that empties the cart (no remaining items, no complement)
// falls through to whole-person suppression.
func TestOnConversion_PerProductClearedCartWholePersonSuppresses(t *testing.T) {
	src := &fakeSource{segs: map[string][]Segment{"adv": {{ID: "seg-a"}}}}
	enr := &fakeEnroller{}
	pv := &fakeProductViews{}
	sup := &fakeSuppression{}
	s := New(src, enr, testLog())
	s.SetProductViews(pv, func() time.Duration { return 30 * 24 * time.Hour })
	s.SetSuppression(sup, nil, func() time.Duration { return 30 * 24 * time.Hour })
	// No complements wired → nothing to rotate to.

	_ = pv.RecordProductViews(context.Background(), "adv", "u1", []string{"SKU-ONLY"}, time.Time{}, 0, "")

	removed, err := s.OnConversion(context.Background(), "adv", "u1", "trace-2", []string{"SKU-ONLY"})
	if err != nil {
		t.Fatalf("OnConversion: %v", err)
	}
	if len(removed) != 1 {
		t.Fatalf("cleared cart should whole-person suppress (removed from segments), got %v", removed)
	}
	if len(sup.personBurns) == 0 {
		t.Errorf("cleared cart should write a whole-person burn")
	}
}

// Household enrollment (anonymous guest carts): with the gate ON and a
// household id on the visit, BOTH the visitor id and the household enroll —
// so the chase reaches the home's other devices with no email bridge. Gate
// off (or nil, the default) keeps the original visitor-only behaviour, and a
// household equal to the user id (household-keyed visitor) never doubles up.
func TestOnSiteVisit_HouseholdEnrollGate(t *testing.T) {
	seg := Segment{ID: "seg-cart", AccountID: "adv", Type: "retargeting", Rule: []byte(`{"event":"site_visit","tag":"cart","min_count":1}`)}
	ev := visitEvent("adv", "guest-1", "cart")
	ev.HouseholdID = "hh:abc123"

	// Gate ON → visitor + household.
	src := &fakeSource{segs: map[string][]Segment{"adv": {seg}}}
	enr := &fakeEnroller{}
	svc := New(src, enr, testLog())
	svc.SetHouseholdEnroll(func() bool { return true })
	if _, err := svc.OnSiteVisit(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if len(enr.added) != 2 || enr.added[0].user != "guest-1" || enr.added[1].user != "hh:abc123" {
		t.Fatalf("want visitor+household enrolled, got %+v", enr.added)
	}

	// Gate OFF → visitor only.
	enr = &fakeEnroller{}
	svc = New(src, enr, testLog())
	svc.SetHouseholdEnroll(func() bool { return false })
	if _, err := svc.OnSiteVisit(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if len(enr.added) != 1 || enr.added[0].user != "guest-1" {
		t.Fatalf("want visitor only, got %+v", enr.added)
	}

	// Household == user id (a household-keyed visitor) must not double-enroll.
	ev2 := ev
	ev2.UserID = "hh:abc123"
	enr = &fakeEnroller{}
	svc = New(src, enr, testLog())
	svc.SetHouseholdEnroll(func() bool { return true })
	if _, err := svc.OnSiteVisit(context.Background(), ev2); err != nil {
		t.Fatal(err)
	}
	if len(enr.added) != 1 {
		t.Fatalf("want single enrollment for hh-keyed visitor, got %+v", enr.added)
	}
}
