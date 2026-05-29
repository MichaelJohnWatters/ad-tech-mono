package fraud

import (
	"time"
)

// ScoreResult is a detailed fraud assessment.
type ScoreResult struct {
	TraceID    string
	Score      float64 // 0.0 (clean) to 1.0 (fraudulent)
	Category   string  // givt, sivt, clean
	Signals    map[string]float64
	Timestamp  time.Time
}

// Scorer combines multiple fraud signals into a single score.
type Scorer struct {
	weights SignalWeights
}

// SignalWeights defines how much each signal contributes to the score.
type SignalWeights struct {
	BotUA          float64 `yaml:"bot_ua"`
	IPBlocklist    float64 `yaml:"ip_blocklist"`
	DataCenterIP   float64 `yaml:"data_center_ip"`
	RateLimited    float64 `yaml:"rate_limited"`
	NoReferer      float64 `yaml:"no_referer"`
	EmptyUA        float64 `yaml:"empty_ua"`
	GeoMismatch    float64 `yaml:"geo_mismatch"`
	SuspiciousTiming float64 `yaml:"suspicious_timing"`
}

// DefaultWeights returns industry-standard signal weights.
func DefaultWeights() SignalWeights {
	return SignalWeights{
		BotUA:            0.90,
		IPBlocklist:      0.95,
		DataCenterIP:     0.40,
		RateLimited:      0.60,
		NoReferer:        0.10,
		EmptyUA:          0.50,
		GeoMismatch:      0.30,
		SuspiciousTiming: 0.25,
	}
}

// NewScorer creates a fraud scorer.
func NewScorer(weights SignalWeights) *Scorer {
	return &Scorer{weights: weights}
}

// Score evaluates a set of fraud signals and produces a composite score.
func (s *Scorer) Score(signals map[string]bool) ScoreResult {
	result := ScoreResult{
		Signals:   make(map[string]float64),
		Timestamp: time.Now(),
	}

	var totalWeight, totalScore float64

	type weightedSignal struct {
		name   string
		weight float64
	}

	checks := []weightedSignal{
		{"bot_ua", s.weights.BotUA},
		{"ip_blocklist", s.weights.IPBlocklist},
		{"data_center_ip", s.weights.DataCenterIP},
		{"rate_limited", s.weights.RateLimited},
		{"no_referer", s.weights.NoReferer},
		{"empty_ua", s.weights.EmptyUA},
		{"geo_mismatch", s.weights.GeoMismatch},
		{"suspicious_timing", s.weights.SuspiciousTiming},
	}

	for _, check := range checks {
		if signals[check.name] {
			result.Signals[check.name] = check.weight
			totalScore += check.weight
		}
		totalWeight += check.weight
	}

	if totalWeight > 0 {
		result.Score = totalScore / totalWeight
	}

	// Clamp
	if result.Score > 1.0 {
		result.Score = 1.0
	}

	// Categorise per IAB IVT guidelines
	switch {
	case result.Score >= 0.7:
		result.Category = "sivt" // Sophisticated Invalid Traffic
	case result.Score >= 0.3:
		result.Category = "givt" // General Invalid Traffic
	default:
		result.Category = "clean"
	}

	return result
}
