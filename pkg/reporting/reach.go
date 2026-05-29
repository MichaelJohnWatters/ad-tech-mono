package reporting

import (
	"encoding/binary"
	"hash/fnv"
	"math"
	"math/bits"
	"sort"
	"sync"
)

// HLL implements a HyperLogLog sketch for cardinality estimation.
// Provides ~2% accuracy using ~12KB of memory regardless of set size.
// Used for reach (unique user counts) across campaigns, geos, devices.
type HLL struct {
	mu        sync.RWMutex
	registers []uint8
	p         uint8  // precision (number of bits for register index)
	m         uint32 // number of registers (2^p)
}

// NewHLL creates a HyperLogLog sketch with the given precision.
// Precision 14 (default) uses 16384 registers = ~16KB, giving ~1% error.
// Precision 12 uses 4096 registers = ~4KB, giving ~1.6% error.
func NewHLL(precision uint8) *HLL {
	if precision < 4 {
		precision = 4
	}
	if precision > 18 {
		precision = 18
	}
	m := uint32(1) << precision
	return &HLL{
		registers: make([]uint8, m),
		p:         precision,
		m:         m,
	}
}

// DefaultHLL creates a HLL with precision 14 (~16KB, ~1% error).
func DefaultHLL() *HLL {
	return NewHLL(14)
}

// Add inserts an element into the sketch.
func (h *HLL) Add(data []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()

	hasher := fnv.New64a()
	hasher.Write(data)
	x := hasher.Sum64()

	// Bottom p bits for register index (FNV has better distribution in low bits)
	idx := x & uint64(h.m-1)
	// Use the remaining (64-p) high bits to determine rho (position of first 1-bit)
	w := x >> uint64(h.p)
	// Count trailing zeros + 1 (equivalent to position of lowest set bit)
	var rho uint8
	if w == 0 {
		rho = uint8(64 - h.p) // max value
	} else {
		rho = uint8(bits.TrailingZeros64(w)) + 1
	}

	if rho > h.registers[idx] {
		h.registers[idx] = rho
	}
}

// AddUint64 inserts a uint64 value (e.g. a pre-hashed ID).
func (h *HLL) AddUint64(val uint64) {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], val)
	h.Add(buf[:])
}

// AddString is a convenience wrapper for string elements.
func (h *HLL) AddString(s string) {
	h.Add([]byte(s))
}

// Count returns the estimated cardinality (number of unique elements).
func (h *HLL) Count() uint64 {
	h.mu.RLock()
	defer h.mu.RUnlock()

	// Harmonic mean of 2^(-register[i])
	var sum float64
	zeros := 0
	for _, val := range h.registers {
		sum += math.Pow(2, -float64(val))
		if val == 0 {
			zeros++
		}
	}

	m := float64(h.m)
	alpha := alphaM(h.m)
	estimate := alpha * m * m / sum

	// Small range correction (linear counting)
	if estimate <= 2.5*m && zeros > 0 {
		estimate = m * math.Log(m/float64(zeros))
	}

	return uint64(estimate + 0.5)
}

// Merge combines another HLL into this one (union operation).
// Both HLLs must have the same precision.
// HLL merge is lossless - the union of two HLLs gives the exact same
// result as if all elements had been added to a single HLL.
func (h *HLL) Merge(other *HLL) {
	if h.p != other.p {
		return // incompatible precisions
	}

	h.mu.Lock()
	other.mu.RLock()
	defer h.mu.Unlock()
	defer other.mu.RUnlock()

	for i := range h.registers {
		if other.registers[i] > h.registers[i] {
			h.registers[i] = other.registers[i]
		}
	}
}

// Clone creates a deep copy of this HLL.
func (h *HLL) Clone() *HLL {
	h.mu.RLock()
	defer h.mu.RUnlock()

	clone := NewHLL(h.p)
	copy(clone.registers, h.registers)
	return clone
}

// IntersectionEstimate estimates |A ∩ B| using inclusion-exclusion.
// |A ∩ B| = |A| + |B| - |A ∪ B|
func IntersectionEstimate(a, b *HLL) uint64 {
	union := a.Clone()
	union.Merge(b)

	countA := a.Count()
	countB := b.Count()
	countUnion := union.Count()

	// Inclusion-exclusion
	intersection := int64(countA) + int64(countB) - int64(countUnion)
	if intersection < 0 {
		return 0
	}
	return uint64(intersection)
}

// alphaM returns the bias correction constant for m registers.
func alphaM(m uint32) float64 {
	switch m {
	case 16:
		return 0.673
	case 32:
		return 0.697
	case 64:
		return 0.709
	default:
		return 0.7213 / (1 + 1.079/float64(m))
	}
}

// FrequencyDistribution tracks how many users saw N impressions.
// Built from per-user impression counters at rollup time.
type FrequencyDistribution struct {
	Buckets map[int]int // frequency -> count of users
	Total   int         // total users
}

// NewFrequencyDistribution creates a distribution from per-user impression counts.
// impressionsPerUser maps user_id -> impression count.
func NewFrequencyDistribution(impressionsPerUser map[string]int) *FrequencyDistribution {
	fd := &FrequencyDistribution{
		Buckets: make(map[int]int),
	}
	for _, count := range impressionsPerUser {
		fd.Buckets[count]++
		fd.Total++
	}
	return fd
}

// AverageFrequency returns the mean number of impressions per user.
func (fd *FrequencyDistribution) AverageFrequency() float64 {
	if fd.Total == 0 {
		return 0
	}
	var totalImpressions int
	for freq, count := range fd.Buckets {
		totalImpressions += freq * count
	}
	return float64(totalImpressions) / float64(fd.Total)
}

// Percentages returns the frequency distribution as percentages.
// Returns sorted buckets: [{Frequency: 1, Percentage: 0.50}, ...]
func (fd *FrequencyDistribution) Percentages() []FrequencyBucket {
	if fd.Total == 0 {
		return nil
	}
	var buckets []FrequencyBucket
	for freq, count := range fd.Buckets {
		buckets = append(buckets, FrequencyBucket{
			Frequency:  freq,
			Count:      count,
			Percentage: float64(count) / float64(fd.Total),
		})
	}
	sort.Slice(buckets, func(i, j int) bool {
		return buckets[i].Frequency < buckets[j].Frequency
	})
	return buckets
}

// FrequencyBucket is a single frequency count in the distribution.
type FrequencyBucket struct {
	Frequency  int
	Count      int
	Percentage float64
}

// ReachForecast estimates campaign reach based on historical data.
type ReachForecast struct {
	EstimatedReach       uint64
	EstimatedImpressions uint64
	EstimatedFrequency   float64
	EstimatedSpend       float64
	EstimatedCPM         float64
	Confidence           string // low, medium, high
}

// ForecastReach estimates reach for a campaign plan.
// Uses historical HLL data and average CPMs from the analytics store.
func ForecastReach(
	budget float64,
	avgCPM float64,
	historicalReach uint64,
	historicalImpressions uint64,
) ReachForecast {
	if avgCPM <= 0 {
		return ReachForecast{Confidence: "low"}
	}

	// Estimate impressions from budget and CPM
	estImpressions := uint64((budget / avgCPM) * 1000)

	// Estimate reach using diminishing returns (logarithmic reach curve).
	// More impressions = higher frequency, not proportional reach.
	var estReach uint64
	if historicalImpressions > 0 {
		scaleFactor := math.Log(float64(estImpressions)+1) / math.Log(float64(historicalImpressions)+1)
		if scaleFactor > 3 {
			scaleFactor = 3 // cap extrapolation
		}
		estReach = uint64(float64(historicalReach) * scaleFactor)
	} else {
		// No history: assume 30% of impressions are unique users
		estReach = uint64(float64(estImpressions) * 0.3)
	}
	if estReach > estImpressions {
		estReach = estImpressions
	}

	var estFreq float64
	if estReach > 0 {
		estFreq = float64(estImpressions) / float64(estReach)
	}

	confidence := "medium"
	if historicalImpressions > 100000 {
		confidence = "high"
	} else if historicalImpressions < 1000 {
		confidence = "low"
	}

	return ReachForecast{
		EstimatedReach:       estReach,
		EstimatedImpressions: estImpressions,
		EstimatedFrequency:   estFreq,
		EstimatedSpend:       budget,
		EstimatedCPM:         avgCPM,
		Confidence:           confidence,
	}
}
