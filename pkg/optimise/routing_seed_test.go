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

// A reseed from a lagging aggregate (present, but still below MinCalls) must
// not un-judge a DSP the pod has already condemned locally — else a slow DSP
// is put back in warm-up every reseed tick and re-called until it relearns
// MinCalls (2026-10-14: the 500ms slowpoke at 20-43% of auctions after a
// reset/boot while dsp_calls ingestion lagged). A WARM aggregate still wins,
// so replicas keep converging on the cluster-global view.
func TestSmartRouter_ReseedLaggingAggregateDoesNotUnjudge(t *testing.T) {
	r := NewSmartRouter()
	slow, ok := "http://slow", "http://ok"
	for i := 0; i < 30; i++ {
		r.RecordCall("display", slow, false, 0, 500e6, true)
		r.RecordCall("display", ok, true, 1, 20e6, false)
	}
	has := func(got []string, want string) bool {
		for _, g := range got {
			if g == want {
				return true
			}
		}
		return false
	}
	if has(r.SelectDSPs("display", []string{slow, ok}), slow) {
		t.Fatalf("precondition: slow DSP should be skipped after 30 timeouts")
	}

	// Lagging aggregate: only 5 rows landed in analytics so far.
	r.Reseed([]DSPStats{{Channel: "display", DSPID: slow, TotalCalls: 5, TotalTimeouts: 4, TimeoutRate: 0.8}})
	if has(r.SelectDSPs("display", []string{slow, ok}), slow) {
		t.Errorf("lagging below-MinCalls aggregate re-admitted a locally-condemned DSP")
	}

	// A warm aggregate replaces local stats as before (here: it recovered).
	r.Reseed([]DSPStats{{Channel: "display", DSPID: slow, TotalCalls: 500, TotalBids: 450, BidRate: 0.9, AvgBid: 2}})
	if !has(r.SelectDSPs("display", []string{slow, ok}), slow) {
		t.Errorf("warm aggregate should still overwrite local stats (cluster convergence)")
	}
}
