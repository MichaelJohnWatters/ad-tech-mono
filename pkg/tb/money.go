package tb

import (
	"math"

	"github.com/tigerbeetle/tigerbeetle-go/pkg/types"
)

// microsPerUSD is the fixed-point scale: 1 USD = 1,000,000 micro-dollars.
// Micros — not cents — because a realized per-impression cost is sub-cent (a
// $5.00 CPM books $0.005 per impression), which cents would truncate to zero.
// Matches the Redis budget tracker (cmd/dsp/budget.go) and the billing pacing
// accumulator (pkg/billing/pacing.go) so a value survives a Redis round-trip
// and a TB round-trip landing on the same micro count.
const microsPerUSD = 1_000_000

// USDToMicros converts a USD float to fixed-point micro-dollars. Rounds to the
// nearest micro (matching pacing's toMicros) so repeated conversions are stable.
func USDToMicros(usd float64) uint64 {
	if usd < 0 {
		return 0
	}
	return uint64(math.Round(usd * microsPerUSD))
}

// MicrosToUSD is the inverse of USDToMicros.
func MicrosToUSD(micros uint64) float64 {
	return float64(micros) / microsPerUSD
}

// MicrosToAmount packs a micro-dollar counter into TB's Uint128 amount field.
// uint128 has room for the full uint64 range with zeroes in the high half.
func MicrosToAmount(micros uint64) types.Uint128 {
	return types.ToUint128(micros)
}

// AmountToMicros extracts a micro-dollar counter from a TB Uint128 amount. The
// high half is asserted to be zero — if a transfer somehow exceeds uint64 micros
// (~$18 trillion) we want to fail loudly rather than truncate silently.
func AmountToMicros(amount types.Uint128) (uint64, bool) {
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
