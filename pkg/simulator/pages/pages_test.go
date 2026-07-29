package pages

import "testing"

// Every layout must have a unique, non-empty slug and at least one slot, and
// every slot must name a format and a placement key. This is the contract the
// demosite render and the e2e replay both rely on.
func TestLayoutsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, l := range All() {
		if l.Slug == "" {
			t.Errorf("layout %q has empty slug", l.Title)
		}
		if seen[l.Slug] {
			t.Errorf("duplicate slug %q", l.Slug)
		}
		seen[l.Slug] = true
		if l.Total() == 0 {
			t.Errorf("layout %q has no slots", l.Slug)
		}
		for i, s := range l.Slots {
			if s.Format == "" || s.PlacementKey == "" {
				t.Errorf("layout %q slot %d missing format/placement: %+v", l.Slug, i, s)
			}
		}
	}
}

// CountByFormat must sum to the slot total — the e2e asserts N impressions
// downstream, so the per-format breakdown must account for every slot.
func TestCountByFormatSumsToTotal(t *testing.T) {
	for _, l := range All() {
		sum := 0
		for _, n := range l.CountByFormat() {
			sum += n
		}
		if sum != l.Total() {
			t.Errorf("layout %q: CountByFormat sums to %d, Total()=%d", l.Slug, sum, l.Total())
		}
	}
}

// Every site must be well-formed: unique slug, a publisher tenant key, at least
// one layout that actually resolves, and a distinct placement per format.
func TestSitesWellFormed(t *testing.T) {
	seenSite := map[string]bool{}
	seenPub := map[string]bool{}
	seenPlacement := map[string]bool{}
	for _, s := range AllSites() {
		if s.Slug == "" || s.Publisher == "" {
			t.Errorf("site %q missing slug/publisher", s.Name)
		}
		if seenSite[s.Slug] {
			t.Errorf("duplicate site slug %q", s.Slug)
		}
		seenSite[s.Slug] = true
		if seenPub[s.Publisher] {
			t.Errorf("two sites share publisher tenant %q — breaks per-tenant attribution", s.Publisher)
		}
		seenPub[s.Publisher] = true
		if s.RevsharePct <= 0 || s.RevsharePct >= 100 {
			t.Errorf("site %q revshare %d%% out of range", s.Slug, s.RevsharePct)
		}
		if len(s.Layouts()) != len(s.LayoutSlugs) {
			t.Errorf("site %q has a layout slug that doesn't resolve", s.Slug)
		}
		// Placement keys must be globally unique across sites (per-tenant inventory).
		for _, key := range s.PlacementByFormat() {
			if seenPlacement[key] {
				t.Errorf("placement key %q reused across sites", key)
			}
			seenPlacement[key] = true
		}
	}
}

func TestBySlug(t *testing.T) {
	if _, ok := BySlug("news-3ad"); !ok {
		t.Fatal("news-3ad should resolve")
	}
	if _, ok := BySlug("does-not-exist"); ok {
		t.Fatal("unknown slug should not resolve")
	}
}
