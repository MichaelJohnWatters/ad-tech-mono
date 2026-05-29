package optimise

import (
	"math"
	"math/rand"
	"sort"
	"sync"
)

// Bandit implements Thompson Sampling for creative rotation.
// Balances exploitation (show what works) with exploration (test unknowns).
type Bandit struct {
	mu   sync.RWMutex
	arms map[string]*Arm
}

// Arm represents one creative variant with its performance data.
type Arm struct {
	ID          string
	Impressions int
	Clicks      int
	Conversions int
	// Beta distribution parameters for Thompson Sampling
	Alpha float64 // successes + 1
	Beta  float64 // failures + 1
}

// NewBandit creates a bandit with the given arm IDs (creative IDs).
func NewBandit(armIDs []string) *Bandit {
	b := &Bandit{arms: make(map[string]*Arm)}
	for _, id := range armIDs {
		b.arms[id] = &Arm{ID: id, Alpha: 1, Beta: 1} // uniform prior
	}
	return b
}

// Select picks which creative to show using Thompson Sampling.
// Each arm draws from its Beta distribution; highest sample wins.
func (b *Bandit) Select() string {
	b.mu.RLock()
	defer b.mu.RUnlock()

	var bestID string
	var bestSample float64

	for id, arm := range b.arms {
		sample := betaSample(arm.Alpha, arm.Beta)
		if sample > bestSample {
			bestSample = sample
			bestID = id
		}
	}
	return bestID
}

// RecordImpression records that a creative was shown.
func (b *Bandit) RecordImpression(armID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if arm, ok := b.arms[armID]; ok {
		arm.Impressions++
		arm.Beta++ // failure until proven otherwise
	}
}

// RecordClick records a click (success) on a creative.
func (b *Bandit) RecordClick(armID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if arm, ok := b.arms[armID]; ok {
		arm.Clicks++
		arm.Alpha++ // success
		arm.Beta--  // undo the failure from impression
		if arm.Beta < 1 {
			arm.Beta = 1
		}
	}
}

// RecordConversion records a conversion on a creative.
func (b *Bandit) RecordConversion(armID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if arm, ok := b.arms[armID]; ok {
		arm.Conversions++
		arm.Alpha += 5 // conversions are worth more than clicks
	}
}

// Weights returns the current traffic allocation weights (0-1 per arm).
func (b *Bandit) Weights() map[string]float64 {
	b.mu.RLock()
	defer b.mu.RUnlock()

	// Run 1000 simulated selections to estimate allocation
	counts := make(map[string]int)
	for i := 0; i < 1000; i++ {
		var bestID string
		var bestSample float64
		for id, arm := range b.arms {
			sample := betaSample(arm.Alpha, arm.Beta)
			if sample > bestSample {
				bestSample = sample
				bestID = id
			}
		}
		counts[bestID]++
	}

	weights := make(map[string]float64)
	for id, count := range counts {
		weights[id] = float64(count) / 1000.0
	}
	return weights
}

// Stats returns performance data for all arms, sorted by CTR.
func (b *Bandit) Stats() []ArmStats {
	b.mu.RLock()
	defer b.mu.RUnlock()

	var stats []ArmStats
	for _, arm := range b.arms {
		ctr := 0.0
		if arm.Impressions > 0 {
			ctr = float64(arm.Clicks) / float64(arm.Impressions) * 100
		}
		stats = append(stats, ArmStats{
			ID:          arm.ID,
			Impressions: arm.Impressions,
			Clicks:      arm.Clicks,
			Conversions: arm.Conversions,
			CTR:         ctr,
			Alpha:       arm.Alpha,
			Beta:        arm.Beta,
		})
	}
	sort.Slice(stats, func(i, j int) bool {
		return stats[i].CTR > stats[j].CTR
	})
	return stats
}

// ArmStats exposes arm performance for reporting.
type ArmStats struct {
	ID          string
	Impressions int
	Clicks      int
	Conversions int
	CTR         float64
	Alpha       float64
	Beta        float64
}

// betaSample draws a random sample from a Beta(alpha, beta) distribution.
// Uses the transformation method for simplicity.
func betaSample(alpha, beta float64) float64 {
	x := gammaVariate(alpha)
	y := gammaVariate(beta)
	if x+y == 0 {
		return 0.5
	}
	return x / (x + y)
}

// gammaVariate generates a gamma-distributed random variable.
// Uses Marsaglia and Tsang's method for alpha >= 1.
func gammaVariate(shape float64) float64 {
	if shape < 1 {
		return gammaVariate(shape+1) * math.Pow(rand.Float64(), 1.0/shape)
	}
	d := shape - 1.0/3.0
	c := 1.0 / math.Sqrt(9.0*d)
	for {
		var x, v float64
		for {
			x = rand.NormFloat64()
			v = 1.0 + c*x
			if v > 0 {
				break
			}
		}
		v = v * v * v
		u := rand.Float64()
		if u < 1.0-0.0331*(x*x)*(x*x) {
			return d * v
		}
		if math.Log(u) < 0.5*x*x+d*(1.0-v+math.Log(v)) {
			return d * v
		}
	}
}
