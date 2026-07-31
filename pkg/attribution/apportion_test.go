package attribution

import (
	"math"
	"testing"
	"time"
)

func sumFractions(cs []Credit) float64 {
	var s float64
	for _, c := range cs {
		s += c.Fraction
	}
	return s
}

func TestApportion(t *testing.T) {
	base := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	conv := base.Add(8 * 24 * time.Hour)
	// Three exposures, deliberately out of order to prove internal sorting.
	chain := []Touchpoint{
		{TraceID: "b", Type: "impression", At: base.Add(4 * 24 * time.Hour)}, // middle
		{TraceID: "c", Type: "click", At: base.Add(7 * 24 * time.Hour)},      // last
		{TraceID: "a", Type: "impression", At: base},                         // first
	}

	t.Run("empty", func(t *testing.T) {
		if got := Apportion(nil, conv, ModelLinear); got != nil {
			t.Fatalf("empty chain: got %v, want nil", got)
		}
	})

	t.Run("last_touch", func(t *testing.T) {
		cs := Apportion(chain, conv, ModelLastTouch)
		if cs[len(cs)-1].Touchpoint.TraceID != "c" || cs[len(cs)-1].Fraction != 1 {
			t.Fatalf("last_touch should give all credit to c: %+v", cs)
		}
		if cs[0].Fraction != 0 {
			t.Errorf("last_touch: oldest should get 0, got %v", cs[0].Fraction)
		}
	})

	t.Run("first_touch", func(t *testing.T) {
		cs := Apportion(chain, conv, ModelFirstTouch)
		if cs[0].Touchpoint.TraceID != "a" || cs[0].Fraction != 1 {
			t.Fatalf("first_touch should give all credit to a (oldest): %+v", cs)
		}
	})

	t.Run("linear", func(t *testing.T) {
		cs := Apportion(chain, conv, ModelLinear)
		for _, c := range cs {
			if math.Abs(c.Fraction-1.0/3.0) > 1e-9 {
				t.Fatalf("linear: each should be 1/3, got %+v", cs)
			}
		}
		// Sorted oldest→newest.
		if cs[0].Touchpoint.TraceID != "a" || cs[2].Touchpoint.TraceID != "c" {
			t.Errorf("linear chain not sorted oldest→newest: %v %v %v", cs[0].Touchpoint.TraceID, cs[1].Touchpoint.TraceID, cs[2].Touchpoint.TraceID)
		}
	})

	t.Run("time_decay recent > older, sums to 1", func(t *testing.T) {
		cs := Apportion(chain, conv, ModelTimeDecay)
		if s := sumFractions(cs); math.Abs(s-1) > 1e-9 {
			t.Fatalf("time_decay must sum to 1, got %v", s)
		}
		// c (most recent) > b > a (oldest).
		if !(cs[2].Fraction > cs[1].Fraction && cs[1].Fraction > cs[0].Fraction) {
			t.Fatalf("time_decay should weight recent higher: %+v", cs)
		}
	})

	t.Run("position_based U-shape", func(t *testing.T) {
		cs := Apportion(chain, conv, ModelPositionBased)
		if math.Abs(cs[0].Fraction-0.4) > 1e-9 || math.Abs(cs[2].Fraction-0.4) > 1e-9 {
			t.Fatalf("position_based: first+last should be 0.4 each: %+v", cs)
		}
		if math.Abs(cs[1].Fraction-0.2) > 1e-9 {
			t.Fatalf("position_based: single middle should be 0.2: %+v", cs)
		}
		if s := sumFractions(cs); math.Abs(s-1) > 1e-9 {
			t.Fatalf("position_based must sum to 1, got %v", s)
		}
	})

	t.Run("single touchpoint gets all under every model", func(t *testing.T) {
		one := []Touchpoint{{TraceID: "solo", At: base}}
		for _, m := range []string{ModelLastTouch, ModelFirstTouch, ModelLinear, ModelTimeDecay, ModelPositionBased} {
			cs := Apportion(one, conv, m)
			if len(cs) != 1 || math.Abs(cs[0].Fraction-1) > 1e-9 {
				t.Fatalf("model %s: single touchpoint should get 1.0, got %+v", m, cs)
			}
		}
	})
}
