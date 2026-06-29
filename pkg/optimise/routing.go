package optimise

import (
	"sort"
	"sync"
	"time"
)

// DSPStats tracks per-(channel, DSP) performance for smart routing.
//
// Stats are segmented by channel because a DSP that's great at display can
// be terrible at video — mixing the two would let a DSP's poor video bid
// rate cause us to skip them on display, where they bid prolifically. Real
// exchanges run per-channel routing models for exactly this reason.
type DSPStats struct {
	Channel       string
	DSPID         string
	BidRate       float64 // % of requests that got a bid
	AvgBid        float64
	AvgLatency    time.Duration
	TimeoutRate   float64
	WinRate       float64
	TotalCalls    int64
	TotalBids     int64
	TotalWins     int64
	TotalTimeouts int64
}

// SmartRouter selects which DSPs to call based on per-channel historical
// performance. Phase 2: skip DSPs likely to no-bid (saves latency). Phase 3:
// rank by expected value (bid_rate × avg_bid × win_rate) with a latency
// penalty.
type SmartRouter struct {
	mu    sync.RWMutex
	stats map[channelDSPKey]*DSPStats
}

type channelDSPKey struct {
	channel string
	dspID   string
}

// NewSmartRouter creates a smart router.
func NewSmartRouter() *SmartRouter {
	return &SmartRouter{stats: make(map[channelDSPKey]*DSPStats)}
}

// RecordCall records a DSP call outcome for a given channel.
func (r *SmartRouter) RecordCall(channel, dspID string, bidReceived bool, bidPrice float64, latency time.Duration, timedOut bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := channelDSPKey{channel: channel, dspID: dspID}
	s, ok := r.stats[key]
	if !ok {
		s = &DSPStats{Channel: channel, DSPID: dspID}
		r.stats[key] = s
	}

	s.TotalCalls++
	if bidReceived {
		s.TotalBids++
		s.AvgBid = (s.AvgBid*float64(s.TotalBids-1) + bidPrice) / float64(s.TotalBids)
	}
	if timedOut {
		s.TotalTimeouts++
	}

	s.BidRate = float64(s.TotalBids) / float64(s.TotalCalls)
	s.TimeoutRate = float64(s.TotalTimeouts) / float64(s.TotalCalls)
	s.AvgLatency = (s.AvgLatency*time.Duration(s.TotalCalls-1) + latency) / time.Duration(s.TotalCalls)
}

// RecordWin records a DSP winning an auction on the given channel.
func (r *SmartRouter) RecordWin(channel, dspID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := channelDSPKey{channel: channel, dspID: dspID}
	if s, ok := r.stats[key]; ok {
		s.TotalWins++
		if s.TotalBids > 0 {
			s.WinRate = float64(s.TotalWins) / float64(s.TotalBids)
		}
	}
}

// SelectDSPs returns the DSPs to call for an auction on the given channel,
// sorted by expected value (best-first). DSPs with no history get a neutral
// score so they aren't starved.
//
// Skip rules (per-channel, after enough samples):
//   - bid_rate < 5% → almost never bids on this channel, skip
//   - timeout_rate > 50% → too slow on this channel, skip
//
// Ranking: expected value = bid_rate × avg_bid (× win_rate if any wins
// recorded). Latency penalty (×0.8 if avg > 50ms, ×0.5 if avg > 80ms) so a
// fast bidder beats a slow bidder of equivalent EV.
func (r *SmartRouter) SelectDSPs(channel string, allDSPs []string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	type scored struct {
		id    string
		score float64
	}

	var candidates []scored
	for _, dspID := range allDSPs {
		key := channelDSPKey{channel: channel, dspID: dspID}
		s, ok := r.stats[key]
		if !ok {
			// No data yet for this (channel, dsp) — include with neutral
			// score so newly added DSPs and untested channels get a fair shot.
			candidates = append(candidates, scored{dspID, 0.5})
			continue
		}

		if s.TotalCalls > 20 && s.BidRate < 0.05 {
			continue
		}
		if s.TotalCalls > 20 && s.TimeoutRate > 0.50 {
			continue
		}

		expectedValue := s.BidRate * s.AvgBid
		if s.WinRate > 0 {
			expectedValue *= s.WinRate
		}
		latencyPenalty := 1.0
		if s.AvgLatency > 50*time.Millisecond {
			latencyPenalty = 0.8
		}
		if s.AvgLatency > 80*time.Millisecond {
			latencyPenalty = 0.5
		}

		candidates = append(candidates, scored{dspID, expectedValue * latencyPenalty})
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	result := make([]string, len(candidates))
	for i, c := range candidates {
		result[i] = c.id
	}
	return result
}

// Preview returns the list of DSPs SelectDSPs would currently return for
// (channel, allDSPs). Read-only — does not record a call. Used by the
// /debug/exchange/routing?preview=true debug endpoint so tests and ops can
// inspect the router's current filter decisions without having to run a
// real auction (which would also mutate stats).
func (r *SmartRouter) Preview(channel string, allDSPs []string) []string {
	return r.SelectDSPs(channel, allDSPs)
}

// Reset clears all learned per-(channel, DSP) stats. Used by the e2e
// suite's smart-router tests so a test can train the router with a known
// sequence of outcomes without inheriting noise from earlier tests in
// the same process. Not exposed to ops by default — wire only behind
// debug.endpoints_enabled.
func (r *SmartRouter) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stats = make(map[channelDSPKey]*DSPStats)
}

// Stats returns all (channel, DSP) stats for debugging. Used by the
// /debug/exchange/routing debug endpoint.
func (r *SmartRouter) Stats() []DSPStats {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var result []DSPStats
	for _, s := range r.stats {
		result = append(result, *s)
	}
	return result
}
