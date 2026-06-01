package deals

import (
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
)

func mkDeal(id, dealType, publisherID string, price float64, opts ...func(*models.Deal)) models.Deal {
	d := models.Deal{
		ID:          id,
		PublisherID: publisherID,
		DealType:    dealType,
		Status:      "active",
		Price:       price,
	}
	for _, opt := range opts {
		opt(&d)
	}
	return d
}

func withAdvertisers(ids ...string) func(*models.Deal) {
	return func(d *models.Deal) { d.AdvertiserIDs = ids }
}
func withPlacements(ids ...string) func(*models.Deal) {
	return func(d *models.Deal) { d.PlacementIDs = ids }
}
func paused(d *models.Deal) { d.Status = "paused" }
func withWindow(start, end time.Time) func(*models.Deal) {
	return func(d *models.Deal) { d.StartDate, d.EndDate = &start, &end }
}

func TestMatcher_PrioritySortPGFirst(t *testing.T) {
	pmp := mkDeal("d-pmp", TypePMP, "pub-1", 1.0)
	pg := mkDeal("d-pg", TypePG, "pub-1", 3.0)
	pref := mkDeal("d-pref", TypePreferred, "pub-1", 2.0)
	m := New([]models.Deal{pmp, pg, pref})

	got := m.Match(Request{PublisherID: "pub-1"})
	if len(got) != 3 {
		t.Fatalf("got %d deals, want 3", len(got))
	}
	if got[0].ID != "d-pg" || got[1].ID != "d-pref" || got[2].ID != "d-pmp" {
		t.Errorf("priority order wrong: %v", []string{got[0].ID, got[1].ID, got[2].ID})
	}
}

func TestMatcher_DifferentPublisherExcluded(t *testing.T) {
	d := mkDeal("d-1", TypePMP, "pub-1", 1.0)
	m := New([]models.Deal{d})
	if got := m.Match(Request{PublisherID: "pub-other"}); len(got) != 0 {
		t.Errorf("expected no matches for different publisher, got %d", len(got))
	}
}

func TestMatcher_PlacementAllowlist(t *testing.T) {
	d := mkDeal("d-1", TypePMP, "pub-1", 1.0, withPlacements("pl-a", "pl-b"))
	m := New([]models.Deal{d})

	if got := m.Match(Request{PublisherID: "pub-1", PlacementID: "pl-a"}); len(got) != 1 {
		t.Errorf("expected match for pl-a, got %d", len(got))
	}
	if got := m.Match(Request{PublisherID: "pub-1", PlacementID: "pl-c"}); len(got) != 0 {
		t.Errorf("expected no match for pl-c, got %d", len(got))
	}
}

func TestMatcher_AdvertiserAllowlist(t *testing.T) {
	d := mkDeal("d-1", TypePMP, "pub-1", 1.0, withAdvertisers("adv-a"))
	m := New([]models.Deal{d})

	if got := m.Match(Request{PublisherID: "pub-1", AdvertiserID: "adv-a"}); len(got) != 1 {
		t.Errorf("allowed advertiser should match")
	}
	if got := m.Match(Request{PublisherID: "pub-1", AdvertiserID: "adv-z"}); len(got) != 0 {
		t.Errorf("disallowed advertiser should not match")
	}
	// Empty advertiser = list-all use case; should bypass the allowlist filter.
	if got := m.Match(Request{PublisherID: "pub-1"}); len(got) != 1 {
		t.Errorf("empty advertiser should bypass allowlist filter")
	}
}

func TestMatcher_PausedExcluded(t *testing.T) {
	d := mkDeal("d-1", TypePMP, "pub-1", 1.0, paused)
	m := New([]models.Deal{d})
	if got := m.Match(Request{PublisherID: "pub-1"}); len(got) != 0 {
		t.Errorf("paused deal should be excluded")
	}
}

func TestMatcher_TimeWindow(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(7 * 24 * time.Hour)
	d := mkDeal("d-1", TypePMP, "pub-1", 1.0, withWindow(t0, t1))
	m := New([]models.Deal{d})

	if got := m.Match(Request{PublisherID: "pub-1", Now: t0.Add(time.Hour)}); len(got) != 1 {
		t.Error("in-window should match")
	}
	if got := m.Match(Request{PublisherID: "pub-1", Now: t0.Add(-time.Hour)}); len(got) != 0 {
		t.Error("before start should not match")
	}
	if got := m.Match(Request{PublisherID: "pub-1", Now: t1.Add(time.Hour)}); len(got) != 0 {
		t.Error("after end should not match")
	}
}

func TestDecide_PGPreempts(t *testing.T) {
	deals := []models.Deal{mkDeal("d-pg", TypePG, "pub-1", 5.0)}
	d := Decide("adv-a", 1.0, deals)
	if d.DealType != TypePG || !d.Preempt {
		t.Errorf("PG should preempt, got %+v", d)
	}
	if d.EffectiveFloor != 5.0 {
		t.Errorf("EffectiveFloor = %v, want 5.0 (deal price exceeds placement floor)", d.EffectiveFloor)
	}
}

func TestDecide_PreferredFloorRespectsPlacementFloor(t *testing.T) {
	deals := []models.Deal{mkDeal("d-pref", TypePreferred, "pub-1", 1.5)}
	d := Decide("adv-a", 2.0, deals)
	if d.DealType != TypePreferred || d.Preempt {
		t.Errorf("Preferred should not preempt, got %+v", d)
	}
	if d.EffectiveFloor != 2.0 {
		t.Errorf("EffectiveFloor = %v, want 2.0 (placement floor exceeds deal price)", d.EffectiveFloor)
	}
}

func TestDecide_NoDealsFallsBackToOpen(t *testing.T) {
	d := Decide("adv-a", 1.0, nil)
	if d.DealType != TypeOpen || d.DealID != "" {
		t.Errorf("no deals should yield Open with empty DealID, got %+v", d)
	}
	if d.EffectiveFloor != 1.0 {
		t.Errorf("EffectiveFloor should equal placement floor, got %v", d.EffectiveFloor)
	}
}
