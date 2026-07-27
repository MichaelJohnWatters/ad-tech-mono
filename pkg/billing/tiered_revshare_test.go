package billing

import "testing"

// TestContractStoreTieredFeeFlipsOnMonthImpressions proves the tier-selection
// wiring: the fee is chosen off the publisher's month-to-date impression count
// (fed separately via SetMonthImpressions), and a warm-cache re-Set of the
// contract config does NOT wipe that count.
func TestContractStoreTieredFeeFlipsOnMonthImpressions(t *testing.T) {
	tiers := []Tier{
		{MinImpressions: 0, MaxImpressions: 100, FeePct: 30},
		{MinImpressions: 100, MaxImpressions: 0, FeePct: 15}, // 0 max = unlimited
	}
	newContract := func() *Contract {
		return &Contract{Model: ModelTiered, Tiers: tiers, Currency: "USD"}
	}

	s := NewContractStore()
	s.Set("pub-1", newContract())

	feeAt := func(imps int64) float64 {
		s.SetMonthImpressions("pub-1", imps)
		return s.Get("pub-1").CalculateRevenue(1.0, "open").FeePercent
	}

	if got := feeAt(50); got != 30 {
		t.Errorf("fee at 50 imps = %v, want 30 (tier 1)", got)
	}
	if got := feeAt(99); got != 30 {
		t.Errorf("fee at 99 imps = %v, want 30 (still tier 1)", got)
	}
	if got := feeAt(100); got != 15 {
		t.Errorf("fee at 100 imps = %v, want 15 (crossed into tier 2)", got)
	}
	if got := feeAt(5000); got != 15 {
		t.Errorf("fee at 5000 imps = %v, want 15 (tier 2 unlimited)", got)
	}

	// A warm-cache re-parse (Set) of the contract config must not reset the
	// count — the two refreshers are independent.
	s.SetMonthImpressions("pub-1", 150)
	s.Set("pub-1", newContract())
	if got := s.Get("pub-1").CalculateRevenue(1.0, "open").FeePercent; got != 15 {
		t.Errorf("fee after contract re-Set = %v, want 15 (count preserved)", got)
	}

	// Get returns a copy — reading it must not mutate the stored contract's
	// MonthImpressions (which stays 0 on the parsed contract).
	s.mu.RLock()
	stored := s.contracts["pub-1"].MonthImpressions
	s.mu.RUnlock()
	if stored != 0 {
		t.Errorf("stored contract MonthImpressions = %d, want 0 (count lives in monthImps, not the contract)", stored)
	}
}
