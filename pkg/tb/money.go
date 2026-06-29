package tb

import "github.com/tigerbeetle/tigerbeetle-go/pkg/types"

// USDToCents converts a USD float to fixed-point cents. Truncates rather
// than rounds, matching the Redis budget tracker's scheme (see
// cmd/dsp/budget.go) so a value that survives a Redis round-trip and a TB
// round-trip lands on the same cent count.
func USDToCents(usd float64) uint64 {
	if usd < 0 {
		return 0
	}
	return uint64(usd * 100)
}

// CentsToUSD is the inverse of USDToCents.
func CentsToUSD(cents uint64) float64 {
	return float64(cents) / 100.0
}

// CentsToAmount packs a cents counter into TB's Uint128 amount field.
// uint128 has room for the full uint64 range with zeroes in the high half.
func CentsToAmount(cents uint64) types.Uint128 {
	return types.ToUint128(cents)
}

// AmountToCents extracts a cents counter from a TB Uint128 amount. The high
// half is asserted to be zero — if a transfer somehow exceeds uint64 cents
// (~$184 quadrillion) we want to fail loudly rather than truncate silently.
func AmountToCents(amount types.Uint128) (uint64, bool) {
	b := amount.Bytes()
	for _, hi := range b[8:] {
		if hi != 0 {
			return 0, false
		}
	}
	var n uint64
	for i := 7; i >= 0; i-- {
		n = (n << 8) | uint64(b[i])
	}
	return n, true
}
