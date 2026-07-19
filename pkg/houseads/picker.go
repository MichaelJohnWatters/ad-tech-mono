package houseads

import "sort"

// Pick selects one ENABLED house ad of the requested format from ads, weighted
// by HouseAd.Weight, and returns (ad, true). If no enabled ad matches the
// format it returns (HouseAd{}, false) — the caller then serves an honest
// no-fill rather than invent content.
//
// Selection is DETERMINISTIC in seed: the same (ads, format, seed) always
// returns the same ad. Math.random / time-based randomness is banned in this
// codebase (nondeterministic tests, unreproducible serving), so the caller
// passes a rotating counter or trace-derived value as the seed and gets
// weighted rotation without a global RNG. Passing distinct seeds across
// requests spreads serving across the eligible ads in proportion to weight.
//
// The candidate set is sorted by ID first so the mapping from seed to ad is
// stable regardless of the input slice order (a warm cache reload may reorder
// rows).
func Pick(ads []HouseAd, format string, seed uint64) (HouseAd, bool) {
	var candidates []HouseAd
	var total int
	for _, a := range ads {
		if a.Format != format || !a.Enabled {
			continue
		}
		w := a.Weight
		if w <= 0 {
			w = 1
		}
		a.Weight = w
		candidates = append(candidates, a)
		total += w
	}
	if len(candidates) == 0 || total <= 0 {
		return HouseAd{}, false
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })

	// Map the seed into [0,total) and walk the cumulative weight bands. Equal
	// weights degenerate to plain round-robin over the sorted candidates.
	pos := int(seed % uint64(total))
	for _, a := range candidates {
		if pos < a.Weight {
			return a, true
		}
		pos -= a.Weight
	}
	// Unreachable (pos < total by construction); return the last candidate to
	// stay total.
	return candidates[len(candidates)-1], true
}
