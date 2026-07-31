// Package attribution holds the model-independent attribution logic shared by
// the reporting service: apportioning a conversion's credit across the chain of
// ad exposures that preceded it (multi-touch attribution). Storage of the chain
// and the billing settle live elsewhere; this package is pure so it's trivially
// testable and reusable.
package attribution

import (
	"math"
	"sort"
	"time"
)

// Attribution models. last_touch is the billing default (one exposure gets all
// the credit); the others are reporting views over the same chain.
const (
	ModelLastTouch     = "last_touch"
	ModelFirstTouch    = "first_touch"
	ModelLinear        = "linear"
	ModelTimeDecay     = "time_decay"
	ModelPositionBased = "position_based"
)

// timeDecayHalfLife is how fast credit falls off with age under time_decay: an
// exposure twice this old before the conversion carries half the weight.
const timeDecayHalfLife = 7 * 24 * time.Hour

// Touchpoint is one ad exposure in a conversion's path.
type Touchpoint struct {
	TraceID string
	Type    string // impression | click | view
	At      time.Time
}

// Credit is one touchpoint's share of a conversion (Fraction in [0,1]); the
// fractions returned for a chain sum to 1 (or 0 for an empty chain).
type Credit struct {
	Touchpoint Touchpoint
	Fraction   float64
}

// Apportion splits one unit of conversion credit across the chain under the
// given model. Input order is irrelevant — the chain is sorted oldest→newest
// internally. An unknown model falls back to last_touch. Empty chain → nil.
func Apportion(chain []Touchpoint, conversionAt time.Time, model string) []Credit {
	n := len(chain)
	if n == 0 {
		return nil
	}
	sorted := make([]Touchpoint, n)
	copy(sorted, chain)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].At.Before(sorted[j].At) })

	weights := make([]float64, n)
	switch model {
	case ModelFirstTouch:
		weights[0] = 1
	case ModelLinear:
		for i := range weights {
			weights[i] = 1
		}
	case ModelTimeDecay:
		for i, tp := range sorted {
			age := conversionAt.Sub(tp.At)
			if age < 0 {
				age = 0
			}
			weights[i] = math.Pow(2, -age.Seconds()/timeDecayHalfLife.Seconds())
		}
	case ModelPositionBased:
		if n == 1 {
			weights[0] = 1
		} else if n == 2 {
			weights[0], weights[1] = 0.5, 0.5
		} else {
			// U-shape: first + last get 40% each, the middle shares 20%.
			weights[0] = 0.4
			weights[n-1] = 0.4
			mid := 0.2 / float64(n-2)
			for i := 1; i < n-1; i++ {
				weights[i] = mid
			}
		}
	default: // ModelLastTouch and unknown
		weights[n-1] = 1
	}

	var total float64
	for _, w := range weights {
		total += w
	}
	out := make([]Credit, n)
	for i, tp := range sorted {
		frac := 0.0
		if total > 0 {
			frac = weights[i] / total
		}
		out[i] = Credit{Touchpoint: tp, Fraction: frac}
	}
	return out
}
