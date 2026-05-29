package optimise

import (
	"sort"
	"sync"
	"time"
)

// DSPStats tracks per-DSP performance for smart routing.
type DSPStats struct {
	DSPID       string
	BidRate     float64 // % of requests that get a bid
	AvgBid      float64
	AvgLatency  time.Duration
	TimeoutRate float64
	WinRate     float64
	TotalCalls  int64
	TotalBids   int64
	TotalWins   int64
	TotalTimeouts int64
}

// SmartRouter selects which DSPs to call based on historical performance.
// Phase 2: skip DSPs likely to no-bid (saves latency).
// Phase 3: weighted routing based on expected value.
type SmartRouter struct {
	mu    sync.RWMutex
	stats map[string]*DSPStats
}

// NewSmartRouter creates a smart router.
func NewSmartRouter() *SmartRouter {
	return &SmartRouter{stats: make(map[string]*DSPStats)}
}

// RecordCall records a DSP call outcome.
func (r *SmartRouter) RecordCall(dspID string, bidReceived bool, bidPrice float64, latency time.Duration, timedOut bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	s, ok := r.stats[dspID]
	if !ok {
		s = &DSPStats{DSPID: dspID}
		r.stats[dspID] = s
	}

	s.TotalCalls++
	if bidReceived {
		s.TotalBids++
		s.AvgBid = (s.AvgBid*float64(s.TotalBids-1) + bidPrice) / float64(s.TotalBids)
	}
	if timedOut {
		s.TotalTimeouts++
	}

	// Update rates
	s.BidRate = float64(s.TotalBids) / float64(s.TotalCalls)
	s.TimeoutRate = float64(s.TotalTimeouts) / float64(s.TotalCalls)
	s.AvgLatency = (s.AvgLatency*time.Duration(s.TotalCalls-1) + latency) / time.Duration(s.TotalCalls)
}

// RecordWin records a DSP winning an auction.
func (r *SmartRouter) RecordWin(dspID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.stats[dspID]; ok {
		s.TotalWins++
		s.WinRate = float64(s.TotalWins) / float64(s.TotalBids)
	}
}

// SelectDSPs returns DSP IDs sorted by expected value.
// Phase 2: skip DSPs with < 5% bid rate or > 50% timeout rate.
// Phase 3: rank by expected value (bid_rate * avg_bid * win_rate).
func (r *SmartRouter) SelectDSPs(allDSPs []string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	type scored struct {
		id    string
		score float64
	}

	var candidates []scored
	for _, dspID := range allDSPs {
		s, ok := r.stats[dspID]
		if !ok {
			// No data yet - include with neutral score
			candidates = append(candidates, scored{dspID, 0.5})
			continue
		}

		// Phase 2: skip clearly unproductive DSPs
		if s.TotalCalls > 20 && s.BidRate < 0.05 {
			continue // almost never bids
		}
		if s.TotalCalls > 20 && s.TimeoutRate > 0.50 {
			continue // times out too often
		}

		// Phase 3: score by expected value
		expectedValue := s.BidRate * s.AvgBid
		if s.WinRate > 0 {
			expectedValue *= s.WinRate
		}
		// Penalise high latency
		latencyPenalty := 1.0
		if s.AvgLatency > 50*time.Millisecond {
			latencyPenalty = 0.8
		}
		if s.AvgLatency > 80*time.Millisecond {
			latencyPenalty = 0.5
		}

		candidates = append(candidates, scored{dspID, expectedValue * latencyPenalty})
	}

	// Sort by score descending
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	result := make([]string, len(candidates))
	for i, c := range candidates {
		result[i] = c.id
	}
	return result
}

// Stats returns all DSP stats for debugging.
func (r *SmartRouter) Stats() []DSPStats {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var result []DSPStats
	for _, s := range r.stats {
		result = append(result, *s)
	}
	return result
}
