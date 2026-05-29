// Package optimise provides optimisation pipelines for bid pricing,
// placement scoring, creative performance, and budget reallocation.
//
// These pipelines consume historical data from the analytics store,
// detect patterns, and feed adjustments back into the live system.
//
// Usage:
//
//	pipeline := optimise.NewBidOptimiser(shadingTracker, logger)
//	adjustments := pipeline.Run(ctx)
package optimise

import (
	"log/slog"
	"math"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/bidshading"
)

// BidAdjustment is a recommended change to a campaign's bid on a placement.
type BidAdjustment struct {
	CampaignID  string
	PlacementID string
	CurrentBid  float64
	SuggestedBid float64
	Reason      string
	Confidence  float64 // 0-1
}

// PlacementScore rates a placement's value for a campaign.
type PlacementScore struct {
	PlacementID   string
	Score         float64 // 0-100
	WinRate       float64
	AvgClearing   float64
	Impressions   int
	Recommendation string
}

// BidOptimiser analyses win/loss data and suggests bid adjustments.
type BidOptimiser struct {
	tracker *bidshading.Tracker
	log     *slog.Logger
}

// NewBidOptimiser creates a bid optimisation pipeline.
func NewBidOptimiser(tracker *bidshading.Tracker, log *slog.Logger) *BidOptimiser {
	return &BidOptimiser{tracker: tracker, log: log}
}

// Run analyses all placements and produces bid adjustments.
func (b *BidOptimiser) Run() []BidAdjustment {
	var adjustments []BidAdjustment

	for _, pid := range b.tracker.AllPlacements() {
		stats := b.tracker.Stats(pid)
		if stats.TotalBids < 10 {
			continue // not enough data
		}

		curve := b.tracker.WinRateCurve(pid)

		// Detect overpaying: win rate > 80% means we're probably bidding too high
		if stats.WinRate > 0.80 && stats.AvgClearing > 0 {
			// Simple heuristic: reduce bid by (win_rate - 0.6) * avg_clearing
			reduction := (stats.WinRate - 0.60) * stats.AvgClearing
			suggested := stats.AvgClearing - reduction
			if suggested < 0 {
				suggested = stats.AvgClearing * 0.5
			}
			// Also try curve-based suggestion
			curveSuggestion := curve.BidForWinRate(0.60)
			if curveSuggestion > 0 && curveSuggestion < suggested {
				suggested = curveSuggestion
			}
			adjustments = append(adjustments, BidAdjustment{
				PlacementID:  pid,
				CurrentBid:   stats.AvgClearing,
				SuggestedBid: suggested,
				Reason:       "overpaying - win rate too high, reduce bid",
				Confidence:   math.Min(float64(stats.TotalBids)/100.0, 1.0),
			})
		}

		// Detect underbidding: win rate < 20% means we're losing too often
		if stats.WinRate < 0.20 && stats.AvgClearing > 0 {
			// Increase bid by 20-30%
			suggested := stats.AvgClearing * 1.25
			curveSuggestion := curve.BidForWinRate(0.50)
			if curveSuggestion > suggested {
				suggested = curveSuggestion
			}
			adjustments = append(adjustments, BidAdjustment{
				PlacementID:  pid,
				CurrentBid:   stats.AvgClearing,
				SuggestedBid: suggested,
				Reason:       "underbidding - win rate too low, increase bid",
				Confidence:   math.Min(float64(stats.TotalBids)/100.0, 1.0),
			})
		}

		// Detect floor issues: many below-floor losses
		if stats.BelowFloor > 0 && float64(stats.BelowFloor)/float64(stats.TotalBids) > 0.3 {
			adjustments = append(adjustments, BidAdjustment{
				PlacementID: pid,
				CurrentBid:  stats.AvgClearing,
				Reason:      "30%+ bids below floor - increase min bid or skip placement",
				Confidence:  0.9,
			})
		}
	}

	b.log.Info("bid optimisation complete", "adjustments", len(adjustments))
	return adjustments
}

// ScorePlacements ranks placements by performance.
func ScorePlacements(tracker *bidshading.Tracker) []PlacementScore {
	var scores []PlacementScore

	for _, pid := range tracker.AllPlacements() {
		stats := tracker.Stats(pid)
		if stats.TotalBids == 0 {
			continue
		}

		// Score based on win rate and clearing efficiency
		winRateScore := stats.WinRate * 40       // 0-40 points
		efficiencyScore := 0.0
		if stats.AvgClearing > 0 {
			// Lower clearing = more efficient (inversely proportional)
			efficiencyScore = math.Min(30, 30.0/(stats.AvgClearing/2.0))
		}
		volumeScore := math.Min(30, float64(stats.TotalBids)/10.0) // 0-30 points

		overall := winRateScore + efficiencyScore + volumeScore

		rec := "standard"
		if overall >= 70 {
			rec = "increase_bid"
		} else if overall < 30 {
			rec = "reduce_bid_or_skip"
		}

		scores = append(scores, PlacementScore{
			PlacementID:    pid,
			Score:          overall,
			WinRate:        stats.WinRate,
			AvgClearing:    stats.AvgClearing,
			Impressions:    stats.TotalBids,
			Recommendation: rec,
		})
	}

	return scores
}
