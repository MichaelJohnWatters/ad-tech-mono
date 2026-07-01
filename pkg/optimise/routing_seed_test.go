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

// SeedArm sets the Beta posterior from historical CTR: a proven high-CTR
// creative gets a strong prior, an unseen one stays uniform.
func TestBandit_SeedArm(t *testing.T) {
	b := NewBandit([]string{"good", "bad", "fresh"})
	b.SeedArm("good", 100, 40)    // 40% CTR
	b.SeedArm("bad", 100, 1)      // 1% CTR
	b.SeedArm("retired", 100, 50) // unknown arm — ignored

	stats := map[string]ArmStats{}
	for _, s := range b.Stats() {
		stats[s.ID] = s
	}
	if stats["good"].Alpha != 41 || stats["good"].Beta != 61 {
		t.Errorf("good arm posterior = a%v/b%v, want 41/61", stats["good"].Alpha, stats["good"].Beta)
	}
	if stats["fresh"].Alpha != 1 || stats["fresh"].Beta != 1 {
		t.Errorf("fresh arm should stay uniform 1/1, got a%v/b%v", stats["fresh"].Alpha, stats["fresh"].Beta)
	}
	if _, ok := stats["retired"]; ok {
		t.Errorf("unknown arm should not be created by SeedArm")
	}
}
