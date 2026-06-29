package tb

import (
	"testing"

	"github.com/tigerbeetle/tigerbeetle-go/pkg/types"
)

func TestUSDToCents(t *testing.T) {
	cases := []struct {
		usd  float64
		want uint64
	}{
		{0, 0},
		{0.01, 1},
		{1.00, 100},
		{1.99, 199},
		{2.50, 250},
		{1234.56, 123456},
		// Truncation, matching cmd/dsp/budget.go.
		{0.005, 0},
		{0.019, 1},
		// Negative clamps to zero — TB Amount is unsigned, so a negative
		// here is a caller bug we don't want to silently overflow to
		// uint64-max.
		{-1.00, 0},
	}
	for _, c := range cases {
		got := USDToCents(c.usd)
		if got != c.want {
			t.Errorf("USDToCents(%v) = %d, want %d", c.usd, got, c.want)
		}
	}
}

func TestCentsToUSDRoundTrip(t *testing.T) {
	cases := []float64{0.00, 0.01, 1.00, 1.99, 1234.56, 9999999.99}
	for _, in := range cases {
		got := CentsToUSD(USDToCents(in))
		if got != in {
			t.Errorf("round-trip %v -> %d -> %v drifted", in, USDToCents(in), got)
		}
	}
}

func TestAmountRoundTrip(t *testing.T) {
	cases := []uint64{0, 1, 100, 123456, 1<<32 - 1, 1 << 40, 1 << 63}
	for _, cents := range cases {
		amt := CentsToAmount(cents)
		got, ok := AmountToCents(amt)
		if !ok {
			t.Errorf("AmountToCents(%d) reported overflow on round-trip", cents)
			continue
		}
		if got != cents {
			t.Errorf("round-trip %d -> %v -> %d", cents, amt, got)
		}
	}
}

func TestAmountToCentsRejectsHighHalf(t *testing.T) {
	var raw [16]byte
	// Smallest possible value that overflows uint64.
	raw[8] = 1
	amt := types.BytesToUint128(raw)
	if _, ok := AmountToCents(amt); ok {
		t.Fatal("expected overflow detection when high half is non-zero")
	}
}
