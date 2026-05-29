package reporting

import (
	"fmt"
	"math"
	"testing"
)

func TestHLL_BasicCardinality(t *testing.T) {
	h := DefaultHLL()

	// Add 100,000 unique elements (better accuracy at higher cardinality)
	n := 100000
	for i := 0; i < n; i++ {
		h.AddString(fmt.Sprintf("user-%d", i))
	}

	count := h.Count()
	// HLL with p=14 should be within ~10% without bias correction
	errorPct := math.Abs(float64(count)-float64(n)) / float64(n)
	if errorPct > 0.10 {
		t.Errorf("count = %d, want ~%d (error %.1f%%)", count, n, errorPct*100)
	}
	t.Logf("HLL estimate: %d, actual: %d, error: %.1f%%", count, n, errorPct*100)
}

func TestHLL_Duplicates(t *testing.T) {
	h := DefaultHLL()

	// Add same element 1000 times
	for i := 0; i < 1000; i++ {
		h.AddString("same-user")
	}

	count := h.Count()
	if count != 1 {
		t.Errorf("count = %d, want 1 (duplicates should not increase count)", count)
	}
}

func TestHLL_Merge(t *testing.T) {
	a := DefaultHLL()
	b := DefaultHLL()

	// Add 50000 to A, 50000 to B (no overlap)
	for i := 0; i < 50000; i++ {
		a.AddString(fmt.Sprintf("a-user-%d", i))
		b.AddString(fmt.Sprintf("b-user-%d", i))
	}

	countA := a.Count()
	countB := b.Count()

	merged := a.Clone()
	merged.Merge(b)
	countMerged := merged.Count()

	// Merged should be roughly A + B (no overlap)
	expected := countA + countB
	errorPct := math.Abs(float64(countMerged)-float64(expected)) / float64(expected)
	if errorPct > 0.10 {
		t.Errorf("merged = %d, want ~%d (error %.1f%%)", countMerged, expected, errorPct*100)
	}
	t.Logf("merge: A=%d B=%d merged=%d expected=%d error=%.1f%%", countA, countB, countMerged, expected, errorPct*100)
}

func TestHLL_MergeWithOverlap(t *testing.T) {
	a := DefaultHLL()
	b := DefaultHLL()

	// Add 50000 shared + 50000 unique each = 150000 total unique
	for i := 0; i < 50000; i++ {
		a.AddString(fmt.Sprintf("shared-user-%d", i))
		b.AddString(fmt.Sprintf("shared-user-%d", i))
	}
	for i := 0; i < 50000; i++ {
		a.AddString(fmt.Sprintf("a-only-%d", i))
		b.AddString(fmt.Sprintf("b-only-%d", i))
	}

	merged := a.Clone()
	merged.Merge(b)
	countMerged := merged.Count()

	expected := 150000
	errorPct := math.Abs(float64(countMerged)-float64(expected)) / float64(expected)
	if errorPct > 0.10 {
		t.Errorf("merged = %d, want ~%d (error %.1f%%)", countMerged, expected, errorPct*100)
	}
	t.Logf("overlap merge: %d vs expected %d, error %.1f%%", countMerged, expected, errorPct*100)
}

func TestIntersectionEstimate(t *testing.T) {
	a := DefaultHLL()
	b := DefaultHLL()

	// 50000 shared users
	for i := 0; i < 50000; i++ {
		a.AddString(fmt.Sprintf("shared-%d", i))
		b.AddString(fmt.Sprintf("shared-%d", i))
	}
	// 50000 unique each
	for i := 0; i < 50000; i++ {
		a.AddString(fmt.Sprintf("a-only-%d", i))
		b.AddString(fmt.Sprintf("b-only-%d", i))
	}

	intersection := IntersectionEstimate(a, b)
	// Intersection via inclusion-exclusion is less precise
	errorPct := math.Abs(float64(intersection)-50000) / 50000
	if errorPct > 0.25 { // intersection estimation compounds error
		t.Errorf("intersection = %d, want ~50000 (error %.1f%%)", intersection, errorPct*100)
	}
	t.Logf("intersection: %d vs expected 50000, error %.1f%%", intersection, errorPct*100)
}

func TestHLL_Empty(t *testing.T) {
	h := DefaultHLL()
	if h.Count() != 0 {
		t.Errorf("empty HLL count = %d, want 0", h.Count())
	}
}

func TestFrequencyDistribution(t *testing.T) {
	impressions := map[string]int{
		"user-1": 1,
		"user-2": 1,
		"user-3": 2,
		"user-4": 3,
		"user-5": 5,
	}

	fd := NewFrequencyDistribution(impressions)

	if fd.Total != 5 {
		t.Errorf("total = %d, want 5", fd.Total)
	}

	avg := fd.AverageFrequency()
	expected := float64(1+1+2+3+5) / 5.0 // 2.4
	if math.Abs(avg-expected) > 0.01 {
		t.Errorf("avg frequency = %.2f, want %.2f", avg, expected)
	}

	pcts := fd.Percentages()
	if len(pcts) == 0 {
		t.Fatal("expected percentages")
	}
	// First bucket should be frequency=1, count=2, 40%
	if pcts[0].Frequency != 1 || pcts[0].Count != 2 {
		t.Errorf("first bucket = {freq:%d, count:%d}, want {1, 2}", pcts[0].Frequency, pcts[0].Count)
	}
}

func TestFrequencyDistribution_Empty(t *testing.T) {
	fd := NewFrequencyDistribution(map[string]int{})
	if fd.AverageFrequency() != 0 {
		t.Error("expected 0 average for empty distribution")
	}
	if fd.Percentages() != nil {
		t.Error("expected nil percentages for empty distribution")
	}
}

func TestForecastReach(t *testing.T) {
	forecast := ForecastReach(
		50000,  // $50K budget
		2.50,   // $2.50 CPM
		200000, // 200K historical reach
		500000, // 500K historical impressions
	)

	if forecast.EstimatedImpressions == 0 {
		t.Error("expected non-zero impressions")
	}
	if forecast.EstimatedReach == 0 {
		t.Error("expected non-zero reach")
	}
	if forecast.EstimatedFrequency <= 0 {
		t.Error("expected positive frequency")
	}
	if forecast.Confidence != "high" {
		t.Errorf("confidence = %s, want high (500K historical impressions)", forecast.Confidence)
	}
}

func TestForecastReach_ZeroCPM(t *testing.T) {
	forecast := ForecastReach(50000, 0, 0, 0)
	if forecast.Confidence != "low" {
		t.Errorf("confidence = %s, want low for zero CPM", forecast.Confidence)
	}
}

func TestForecastReach_LowHistory(t *testing.T) {
	forecast := ForecastReach(10000, 3.00, 100, 500)
	if forecast.Confidence != "low" {
		t.Errorf("confidence = %s, want low for small historical data", forecast.Confidence)
	}
}
