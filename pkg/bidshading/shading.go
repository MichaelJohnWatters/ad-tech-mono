// Package bidshading tracks win/loss data per placement and builds
// win-rate curves for bid price optimisation.
//
// When we bid $X on a placement and lose, we learn the clearing price was
// higher. When we win, we may have overpaid. Over time, this data builds
// a curve: "to win Y% of the time on this placement, bid $Z."
//
// Usage:
//
//	tracker := bidshading.NewTracker()
//	tracker.RecordWin("pl-1", 2.50, 2.30)
//	tracker.RecordLoss("pl-1", 2.00, 3.00, bidshading.ReasonOutbid)
//	curve := tracker.WinRateCurve("pl-1")
//	optimalBid := curve.BidForWinRate(0.60) // bid to win 60% of the time
package bidshading

import (
	"math"
	"sync"
)

// LossReason is the OpenRTB loss reason code.
type LossReason int

const (
	ReasonBelowFloor LossReason = 100
	ReasonOutbid     LossReason = 102
	ReasonBlocked    LossReason = 103
	ReasonCreative   LossReason = 104
	ReasonTimeout    LossReason = 2
)

// Record is a single win or loss data point.
type Record struct {
	OurBid        float64
	ClearingPrice float64
	Won           bool
	Reason        LossReason // only for losses
}

// Tracker accumulates win/loss data per placement and campaign.
type Tracker struct {
	mu   sync.RWMutex
	data map[string][]Record // key: "placement_id" or "placement_id:campaign_id"
}

// NewTracker creates a win/loss tracker.
func NewTracker() *Tracker {
	return &Tracker{data: make(map[string][]Record)}
}

// RecordWin records a winning bid.
func (t *Tracker) RecordWin(placementID string, ourBid, clearingPrice float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data[placementID] = append(t.data[placementID], Record{
		OurBid:        ourBid,
		ClearingPrice: clearingPrice,
		Won:           true,
	})
}

// RecordLoss records a losing bid.
func (t *Tracker) RecordLoss(placementID string, ourBid, clearingPrice float64, reason LossReason) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data[placementID] = append(t.data[placementID], Record{
		OurBid:        ourBid,
		ClearingPrice: clearingPrice,
		Won:           false,
		Reason:        reason,
	})
}

// Stats returns aggregate stats for a placement.
func (t *Tracker) Stats(placementID string) PlacementStats {
	t.mu.RLock()
	defer t.mu.RUnlock()

	records := t.data[placementID]
	if len(records) == 0 {
		return PlacementStats{}
	}

	var stats PlacementStats
	var totalClearing float64
	for _, r := range records {
		stats.TotalBids++
		totalClearing += r.ClearingPrice
		if r.Won {
			stats.Wins++
		} else {
			stats.Losses++
			switch r.Reason {
			case ReasonBelowFloor:
				stats.BelowFloor++
			case ReasonOutbid:
				stats.Outbid++
			}
		}
	}
	stats.WinRate = float64(stats.Wins) / float64(stats.TotalBids)
	stats.AvgClearing = totalClearing / float64(stats.TotalBids)
	return stats
}

// PlacementStats summarises win/loss performance for a placement.
type PlacementStats struct {
	TotalBids   int
	Wins        int
	Losses      int
	WinRate     float64
	AvgClearing float64
	BelowFloor  int
	Outbid      int
}

// WinRateCurve builds a win-rate curve for a placement.
// Groups bids into price buckets and calculates win rate at each level.
func (t *Tracker) WinRateCurve(placementID string) Curve {
	t.mu.RLock()
	defer t.mu.RUnlock()

	records := t.data[placementID]
	if len(records) == 0 {
		return Curve{}
	}

	// Bucket bids by price (round to nearest 0.50)
	type bucket struct {
		wins, total int
	}
	buckets := make(map[float64]*bucket)

	for _, r := range records {
		price := math.Round(r.OurBid*2) / 2 // round to nearest 0.50
		b, ok := buckets[price]
		if !ok {
			b = &bucket{}
			buckets[price] = b
		}
		b.total++
		if r.Won {
			b.wins++
		}
	}

	var points []CurvePoint
	for price, b := range buckets {
		points = append(points, CurvePoint{
			BidPrice: price,
			WinRate:  float64(b.wins) / float64(b.total),
			Samples:  b.total,
		})
	}

	// Estimate midpoint (50% win rate) via linear interpolation
	var midpoint float64
	if len(records) > 0 {
		stats := t.Stats(placementID)
		midpoint = stats.AvgClearing
	}

	return Curve{Points: points, Midpoint: midpoint}
}

// AllPlacements returns all placement IDs with data.
func (t *Tracker) AllPlacements() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var ids []string
	for k := range t.data {
		ids = append(ids, k)
	}
	return ids
}

// Curve represents a win-rate curve for a placement.
type Curve struct {
	Points   []CurvePoint
	Midpoint float64 // estimated bid price for 50% win rate
}

// CurvePoint is a single data point on the win-rate curve.
type CurvePoint struct {
	BidPrice float64
	WinRate  float64
	Samples  int
}

// BidForWinRate returns the estimated bid price to achieve the target win rate.
// Uses the midpoint and a logistic model approximation.
func (c Curve) BidForWinRate(targetRate float64) float64 {
	if c.Midpoint <= 0 || len(c.Points) == 0 {
		return 0 // not enough data
	}
	if targetRate <= 0 {
		return 0
	}
	if targetRate >= 1 {
		targetRate = 0.99
	}

	// Logistic model: win_rate = 1 / (1 + exp(-k * (bid - midpoint)))
	// Solve for bid: bid = midpoint - ln((1/target) - 1) / k
	// Estimate k from data spread (steeper = more competitive placement)
	k := 2.0 // default steepness
	if len(c.Points) >= 2 {
		// Rough k estimate from data range
		var minP, maxP float64 = c.Points[0].BidPrice, c.Points[0].BidPrice
		for _, p := range c.Points {
			if p.BidPrice < minP {
				minP = p.BidPrice
			}
			if p.BidPrice > maxP {
				maxP = p.BidPrice
			}
		}
		spread := maxP - minP
		if spread > 0 {
			k = 4.0 / spread // steepness inversely proportional to price spread
		}
	}

	bid := c.Midpoint - math.Log((1.0/targetRate)-1)/k
	if bid < 0 {
		return 0
	}
	return bid
}
