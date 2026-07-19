package houseads

import "testing"

func mkAd(id, format string, enabled bool, weight int) HouseAd {
	return HouseAd{ID: id, Format: format, Name: id, Markup: "<x/>", Enabled: enabled, Weight: weight}
}

// TestPickEmptyAndFormatMiss: no ads, no enabled ad, or no ad of the format all
// return found=false so the caller serves an honest no-fill.
func TestPickEmptyAndFormatMiss(t *testing.T) {
	if _, ok := Pick(nil, FormatVideo, 0); ok {
		t.Error("empty set must return found=false")
	}
	ads := []HouseAd{
		mkAd("a", FormatVideo, false, 1), // disabled
		mkAd("b", FormatAudio, true, 1),  // wrong format
	}
	if _, ok := Pick(ads, FormatVideo, 0); ok {
		t.Error("no enabled video ad must return found=false")
	}
	if ad, ok := Pick(ads, FormatAudio, 0); !ok || ad.ID != "b" {
		t.Errorf("want the enabled audio ad b, got %+v ok=%v", ad, ok)
	}
}

// TestPickDeterministic: the same (ads, format, seed) always returns the same
// ad — no time/rand, so tests are reproducible.
func TestPickDeterministic(t *testing.T) {
	ads := []HouseAd{
		mkAd("a", FormatVideo, true, 1),
		mkAd("b", FormatVideo, true, 1),
		mkAd("c", FormatVideo, true, 1),
	}
	for seed := uint64(0); seed < 100; seed++ {
		first, _ := Pick(ads, FormatVideo, seed)
		second, _ := Pick(ads, FormatVideo, seed)
		if first.ID != second.ID {
			t.Fatalf("seed %d nondeterministic: %s vs %s", seed, first.ID, second.ID)
		}
	}
}

// TestPickWeighting: over the seed space, selection frequency tracks weight. An
// ad with weight 3 out of a total of 5 should be picked ~3/5 of the time.
func TestPickWeighting(t *testing.T) {
	ads := []HouseAd{
		mkAd("heavy", FormatVideo, true, 3),
		mkAd("light", FormatVideo, true, 2),
	}
	counts := map[string]int{}
	const n = 5000
	for seed := uint64(0); seed < n; seed++ {
		ad, ok := Pick(ads, FormatVideo, seed)
		if !ok {
			t.Fatalf("seed %d: no pick", seed)
		}
		counts[ad.ID]++
	}
	// Seeds are uniform mod 5, so heavy:light ≈ 3:2 exactly across a multiple
	// of 5. Allow a small slack for the non-multiple tail.
	heavyFrac := float64(counts["heavy"]) / float64(n)
	if heavyFrac < 0.58 || heavyFrac > 0.62 {
		t.Errorf("heavy fraction = %.3f, want ~0.60 (weight 3/5); counts=%v", heavyFrac, counts)
	}
}

// TestPickIgnoresInputOrder: the seed→ad mapping is stable regardless of the
// input slice order (a warm-cache reload may reorder rows).
func TestPickIgnoresInputOrder(t *testing.T) {
	forward := []HouseAd{
		mkAd("a", FormatVideo, true, 1),
		mkAd("b", FormatVideo, true, 1),
		mkAd("c", FormatVideo, true, 1),
	}
	reversed := []HouseAd{forward[2], forward[1], forward[0]}
	for seed := uint64(0); seed < 50; seed++ {
		f, _ := Pick(forward, FormatVideo, seed)
		r, _ := Pick(reversed, FormatVideo, seed)
		if f.ID != r.ID {
			t.Fatalf("seed %d order-dependent: %s vs %s", seed, f.ID, r.ID)
		}
	}
}

// TestPickZeroWeightDefaultsToOne: a weight <= 0 is treated as 1 so a
// misconfigured ad still participates rather than dividing by zero.
func TestPickZeroWeightDefaultsToOne(t *testing.T) {
	ads := []HouseAd{mkAd("a", FormatVideo, true, 0)}
	if ad, ok := Pick(ads, FormatVideo, 7); !ok || ad.ID != "a" {
		t.Errorf("zero-weight enabled ad must still be pickable, got %+v ok=%v", ad, ok)
	}
}
