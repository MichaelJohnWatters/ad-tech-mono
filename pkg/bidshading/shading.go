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

// TargetWinRate maps a per-line-item shading mode to the win rate the DSP aims to
// HOLD while shading the bid down toward the clearing price. disabled / empty /
// unknown → 0 (no shading). More aggressive = lower target = lower bid = more
// saving but fewer wins.
func TargetWinRate(mode string) float64 {
	switch mode {
	case "conservative":
		return 0.90
	case "moderate":
		return 0.80
	case "aggressive":
		return 0.65
	}
	return 0
}

// ShadedBid returns the price to submit for a valuation under a shading mode,
// given the placement's win-rate curve. Invariants: never bids ABOVE the valuation
// (never pay more than the impression is worth) and never BELOW the floor. Returns
// the valuation unchanged when shading is off, the curve has no data, or the
// curve's estimate for the target win rate is already ≥ the valuation (market too
// expensive to shade). The realized saving is valuation − ShadedBid (≥ 0).
func ShadedBid(c Curve, mode string, valuation, floor float64) float64 {
	target := TargetWinRate(mode)
	if target <= 0 || c.Midpoint <= 0 || valuation <= 0 {
		return valuation
	}
	want := c.BidForWinRate(target)
	if want <= 0 || want >= valuation {
		return valuation // already bidding at/below the curve target — nothing to shade
	}
	if want < floor {
		want = floor // can't shade below the floor
	}
	if want >= valuation {
		return valuation
	}
	return want
}

// Record is a single win or loss data point.
type Record struct {
	OurBid        float64
	ClearingPrice float64
	Won           bool
	Reason        LossReason // only for losses
	// Savings is the realized bid-shading saving on a WIN: the pre-shade valuation
	// minus the (shaded) price bid/paid, in CPM. 0 when shading wasn't applied or
	// on losses.
	Savings float64
}

// Tracker accumulates win/loss data per placement and campaign.
//
// `data` is the POOLED per-placement tally that DRIVES bid shading — aggregated
// across every advertiser our DSP bids for on that placement (lots of data →
// fast curve convergence; this is what the bid loop reads). `byAdv` is a SECOND,
// reporting-only tally keyed by (placement, advertiser) so the advertiser portal
// can show each advertiser its OWN win/loss without changing the bid decision.
// Both are in-memory map appends on the win/loss nurl path — no hot-path I/O.
type Tracker struct {
	mu    sync.RWMutex
	data  map[string][]Record // pooled, key: placement_id — drives shading
	byAdv map[string][]Record // reporting, key: advKey(placement, advertiser)
}

// NewTracker creates a win/loss tracker.
func NewTracker() *Tracker {
	return &Tracker{data: make(map[string][]Record), byAdv: make(map[string][]Record)}
}

// advKey composes the (placement, advertiser) reporting key. The NUL separator
// can't appear in an id, so there's no ambiguity between the two components.
func advKey(placementID, advertiserID string) string {
	return placementID + "\x00" + advertiserID
}

// maxRecordsPerKey bounds each per-key history so the tracker can't grow without
// limit over a long-running pod (RecordWin/RecordLoss used to append forever).
// It also keeps the win-rate curve RECENT — stale auctions shouldn't weight
// today's bid. Plenty of samples for a stable curve.
const maxRecordsPerKey = 2000

// appendBounded appends r, keeping at most maxRecordsPerKey of the most recent
// records. When full it compacts to the recent half in a fresh slice (freeing the
// old backing array) — amortised O(1) per append, hard memory bound per key.
func appendBounded(s []Record, r Record) []Record {
	if len(s) >= maxRecordsPerKey {
		s = append(make([]Record, 0, maxRecordsPerKey), s[maxRecordsPerKey/2:]...)
	}
	return append(s, r)
}

// RecordWin records a winning bid. advertiserID (the winning campaign's account)
// may be "" — then only the pooled per-placement tally is updated.
func (t *Tracker) RecordWin(placementID, advertiserID string, ourBid, clearingPrice float64) {
	t.recordWin(placementID, advertiserID, ourBid, clearingPrice, 0)
}

// RecordWinShaded is RecordWin plus the realized shading saving (pre-shade
// valuation − shaded price, CPM) so the advertiser's true "dollars saved" can be
// summed from the per-advertiser tally.
func (t *Tracker) RecordWinShaded(placementID, advertiserID string, ourBid, clearingPrice, savings float64) {
	t.recordWin(placementID, advertiserID, ourBid, clearingPrice, savings)
}

func (t *Tracker) recordWin(placementID, advertiserID string, ourBid, clearingPrice, savings float64) {
	rec := Record{OurBid: ourBid, ClearingPrice: clearingPrice, Won: true, Savings: savings}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data[placementID] = appendBounded(t.data[placementID], rec)
	if advertiserID != "" {
		k := advKey(placementID, advertiserID)
		t.byAdv[k] = appendBounded(t.byAdv[k], rec)
	}
}

// RecordLoss records a losing bid. advertiserID may be "" (pooled tally only).
func (t *Tracker) RecordLoss(placementID, advertiserID string, ourBid, clearingPrice float64, reason LossReason) {
	rec := Record{OurBid: ourBid, ClearingPrice: clearingPrice, Won: false, Reason: reason}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data[placementID] = appendBounded(t.data[placementID], rec)
	if advertiserID != "" {
		k := advKey(placementID, advertiserID)
		t.byAdv[k] = appendBounded(t.byAdv[k], rec)
	}
}

// statsFromRecords aggregates a record slice. Shared by the pooled Stats and the
// per-advertiser StatsFor so the two can never diverge in how they count.
func statsFromRecords(records []Record) PlacementStats {
	if len(records) == 0 {
		return PlacementStats{}
	}
	var stats PlacementStats
	var totalClearing float64
	for _, r := range records {
		stats.TotalBids++
		totalClearing += r.ClearingPrice
		stats.TotalSavings += r.Savings
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

// Stats returns POOLED aggregate stats for a placement (the shading view).
func (t *Tracker) Stats(placementID string) PlacementStats {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return statsFromRecords(t.data[placementID])
}

// StatsFor returns one advertiser's OWN aggregate stats for a placement —
// reporting only (this does NOT feed the bid decision). Empty if that advertiser
// has no recorded bids on the placement.
func (t *Tracker) StatsFor(placementID, advertiserID string) PlacementStats {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return statsFromRecords(t.byAdv[advKey(placementID, advertiserID)])
}

// AdvertiserStats returns every placement an advertiser has bid data on, mapped
// to that advertiser's own stats. Powers the per-advertiser portal view.
func (t *Tracker) AdvertiserStats(advertiserID string) map[string]PlacementStats {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[string]PlacementStats)
	suffix := "\x00" + advertiserID
	for k, recs := range t.byAdv {
		if len(k) > len(suffix) && k[len(k)-len(suffix):] == suffix {
			placementID := k[:len(k)-len(suffix)]
			out[placementID] = statsFromRecords(recs)
		}
	}
	return out
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
	// TotalSavings is the summed realized bid-shading saving (CPM) across the wins
	// in this tally — pre-shade valuation minus shaded price. 0 when no shading.
	TotalSavings float64
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
