package optimise

import "testing"

// A warm-start seed drives routing immediately: a DSP seeded as a persistent
// no-bidder is skipped, an unseen DSP is still included.
func TestSmartRouter_SeedDrivesRouting(t *testing.T) {
	r := NewSmartRouter()
	r.Seed([]DSPStats{
		{Channel: "display", DSPID: "http://dspA", TotalCalls: 30, TotalBids: 0, BidRate: 0.0},
	})
	got := r.SelectDSPs("display", []string{"http://dspA", "http://dspB"})

	seen := map[string]bool{}
	for _, e := range got {
		seen[e] = true
	}
	if seen["http://dspA"] {
		t.Errorf("dspA (seeded persistent no-bidder) should be skipped, got %v", got)
	}
	if !seen["http://dspB"] {
		t.Errorf("dspB (unseen, neutral) should be included, got %v", got)
	}
}
