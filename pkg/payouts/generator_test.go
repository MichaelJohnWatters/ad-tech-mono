package payouts

import (
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/billing"
)

func TestComputePayout(t *testing.T) {
	fixed20 := &billing.Contract{Model: billing.ModelFixed, FeePct: 20, Currency: "USD"}

	cases := []struct {
		name       string
		gross      float64
		contract   *billing.Contract
		minCents   int64
		wantAmount float64
		wantFee    float64
		wantWrite  bool
	}{
		{
			name: "fixed 20% fee, above minimum", gross: 10, contract: fixed20, minCents: 0,
			wantAmount: 8.00, wantFee: 2.00, wantWrite: true,
		},
		{
			name: "net below minimum threshold → held, not written",
			// gross $1 → net $0.80 = 80 cents; min $100 (10000 cents) → held.
			gross: 1, contract: fixed20, minCents: 10000,
			wantAmount: 0.80, wantFee: 0.20, wantWrite: false,
		},
		{
			name: "net exactly meets minimum → written",
			// gross $1.25 → net $1.00 = 100 cents; min 100 cents → written.
			gross: 1.25, contract: fixed20, minCents: 100,
			wantAmount: 1.00, wantFee: 0.25, wantWrite: true,
		},
		{
			name: "zero gross → not written", gross: 0, contract: fixed20, minCents: 0,
			wantAmount: 0, wantFee: 0, wantWrite: false,
		},
		{
			name: "rounds to cents (no float dust)",
			// gross $0.333 → net 0.2664 → rounds to $0.27, fee 0.0666 → $0.07.
			gross: 0.333, contract: fixed20, minCents: 0,
			wantAmount: 0.27, wantFee: 0.07, wantWrite: true,
		},
		{
			name: "guaranteed minimum floor lifts net (subsidy absorbed)",
			// GuaranteedMinCPM 5000 → 5.0/imp floor; fixed 90% fee would net
			// 0.10/imp on gross 1.0, floored up to 5.0 (subsidy 4.90).
			gross:    1.0,
			contract: &billing.Contract{Model: billing.ModelFixed, FeePct: 90, GuaranteedMinCPM: 5000, Currency: "USD"},
			minCents: 0, wantAmount: 5.00, wantFee: -4.00, wantWrite: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			amount, fee, write := ComputePayout(c.gross, c.contract, c.minCents)
			if amount != c.wantAmount {
				t.Errorf("amount = %.4f, want %.4f", amount, c.wantAmount)
			}
			if fee != c.wantFee {
				t.Errorf("fee = %.4f, want %.4f", fee, c.wantFee)
			}
			if write != c.wantWrite {
				t.Errorf("write = %v, want %v", write, c.wantWrite)
			}
		})
	}
}
