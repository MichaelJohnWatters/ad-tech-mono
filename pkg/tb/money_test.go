package tb

import (
	"testing"

	"github.com/tigerbeetle/tigerbeetle-go/pkg/types"
)

func TestUSDToMicros(t *testing.T) {
	cases := []struct {
		usd  float64
		want uint64
	}{
		{0, 0},
		{0.01, 10_000},
		{1.00, 1_000_000},
		{1.99, 1_990_000},
		{2.50, 2_500_000},
		{1234.56, 1_234_560_000},
		// Sub-cent per-impression costs are represented exactly in micros —
		// this is the whole point of micros over cents (a $5 CPM = $0.005).
		{0.005, 5_000},
		{0.019, 19_000},
		// Negative clamps to zero — TB Amount is unsigned, so a negative
		// here is a caller bug we don't want to silently overflow to
		// uint64-max.
		{-1.00, 0},
	}
	for _, c := range cases {
		got := USDToMicros(c.usd)
		if got != c.want {
			t.Errorf("USDToMicros(%v) = %d, want %d", c.usd, got, c.want)
		}
	}
}

func TestMicrosToUSDRoundTrip(t *testing.T) {
	cases := []float64{0.00, 0.005, 0.01, 1.00, 1.99, 1234.56, 9999999.99}
	for _, in := range cases {
		got := MicrosToUSD(USDToMicros(in))
		if got != in {
			t.Errorf("round-trip %v -> %d -> %v drifted", in, USDToMicros(in), got)
		}
	}
}

func TestAmountRoundTrip(t *testing.T) {
	cases := []uint64{0, 1, 100, 123456, 1<<32 - 1, 1 << 40, 1 << 63}
	for _, micros := range cases {
		amt := MicrosToAmount(micros)
		got, ok := AmountToMicros(amt)
		if !ok {
			t.Errorf("AmountToMicros(%d) reported overflow on round-trip", micros)
			continue
		}
		if got != micros {
			t.Errorf("round-trip %d -> %v -> %d", micros, amt, got)
		}
	}
}

func TestAmountToMicrosRejectsHighHalf(t *testing.T) {
	var raw [16]byte
	// Smallest possible value that overflows uint64.
	raw[8] = 1
	amt := types.BytesToUint128(raw)
	if _, ok := AmountToMicros(amt); ok {
		t.Fatal("expected overflow detection when high half is non-zero")
	}
}
