package targeting

// QualityScore rates how valuable a placement is for bid decisions.
// Higher score = higher quality inventory = bid more aggressively.
type QualityScore struct {
	PlacementID    string
	Overall        float64 // 0-100
	Viewability    float64 // 0-100 (% of impressions that were viewable)
	CTR            float64 // 0-100 (click-through rate percentile)
	FraudRate      float64 // 0-100 (100 = no fraud)
	BrandSafety    float64 // 0-100
	AdDensity      float64 // 0-100 (100 = low density = good)
}

// ComputeQualityScore calculates an overall quality score from individual metrics.
func ComputeQualityScore(viewabilityRate, ctr, fraudRate, brandSafetyRate, adDensityScore float64) QualityScore {
	// Weights for each component
	viewabilityW := 0.30
	ctrW := 0.20
	fraudW := 0.25
	brandSafetyW := 0.15
	adDensityW := 0.10

	overall := viewabilityRate*viewabilityW +
		ctr*ctrW +
		(100-fraudRate)*fraudW + // invert: low fraud = high score
		brandSafetyRate*brandSafetyW +
		adDensityScore*adDensityW

	return QualityScore{
		Overall:     clamp(overall, 0, 100),
		Viewability: viewabilityRate,
		CTR:         ctr,
		FraudRate:   fraudRate,
		BrandSafety: brandSafetyRate,
		AdDensity:   adDensityScore,
	}
}

// BidAdjustment returns a bid multiplier based on quality score.
// High quality = bid more, low quality = bid less or skip.
func (q QualityScore) BidAdjustment() float64 {
	switch {
	case q.Overall >= 80:
		return 1.20 // +20% for premium inventory
	case q.Overall >= 60:
		return 1.00 // standard
	case q.Overall >= 40:
		return 0.80 // -20% for below average
	case q.Overall >= 20:
		return 0.50 // -50% for poor quality
	default:
		return 0.0 // don't bid on very low quality
	}
}

func clamp(v, min, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
