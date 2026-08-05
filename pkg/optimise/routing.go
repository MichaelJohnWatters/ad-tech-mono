package optimise

import (
	"hash/fnv"
	"sort"
	"sync"
	"time"
)

// Knobs are the SmartRouter's tunable thresholds. The exchange wires them
// to live config (exchange.routing_*) via SetKnobs so ops can tune skip
// behaviour from the staff portal without a deploy; the zero-value-free
// defaults below are the previously hardcoded numbers.
type Knobs struct {
	// Enabled=false is the kill-switch: SelectDSPs returns the input list
	// unchanged (no skips, no ranking). For routing misbehaviour or while
	// onboarding a DSP that must not be throttled during ramp-up.
	Enabled bool
	// MinCalls is the per-(channel, DSP) sample size before the skip rules
	// may act — thin stats never exclude anyone.
	MinCalls int64
	// MinBidRate skips a DSP whose bid rate is below this (default 5%).
	MinBidRate float64
	// MaxTimeoutRate skips a DSP whose timeout rate exceeds this (default 50%).
	MaxTimeoutRate float64
	// ExplorePct (0-100) is the ε-probe: a skip-filtered DSP is still called
	// on this percentage of auctions (deterministic per trace), so its stats
	// keep flowing and a DSP that recovers earns its way back in within
	// minutes instead of staying blacklisted until the next pod restart.
	// 0 disables exploration (skips become sticky for the pod's lifetime).
	ExplorePct float64
	// NeverSkip lists DSP endpoints the skip rules must never exclude —
	// e.g. a DSP holding PG/PMP deals, whose deal bids would silently
	// starve if its open-market bid rate got it routed out.
	NeverSkip map[string]struct{}
	// LatencySoft/LatencyHard are the ranking penalty thresholds:
	// avg latency above soft multiplies the score ×0.8, above hard ×0.5.
	LatencySoft time.Duration
	LatencyHard time.Duration
	// RecencyWindow is the effective sample horizon (in calls) for the
	// rolling stats. Stats were lifetime cumulative averages, which made the
	// ε-probe's "earns its way back in" promise hollow: after 100k bad
	// calls, a recovered DSP's cumulative bid rate needs ~forever at a 1%
	// probe rate to climb back over MinBidRate. With an EWMA horizon of N
	// calls, behaviour changes show in ~N samples regardless of history —
	// a skipped-then-recovered DSP rehabilitates in minutes of probing.
	RecencyWindow int64
}

// DefaultKnobs returns the historical hardcoded thresholds.
func DefaultKnobs() Knobs {
	return Knobs{
		Enabled:        true,
		MinCalls:       20,
		MinBidRate:     0.05,
		MaxTimeoutRate: 0.50,
		ExplorePct:     1,
		LatencySoft:    50 * time.Millisecond,
		LatencyHard:    80 * time.Millisecond,
		RecencyWindow:  200,
	}
}

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

	// knobsFn supplies live thresholds per selection; nil = DefaultKnobs.
	knobsFn func() Knobs
}

type channelDSPKey struct {
	channel string
	dspID   string
}

// NewSmartRouter creates a smart router with DefaultKnobs.
func NewSmartRouter() *SmartRouter {
	return &SmartRouter{stats: make(map[channelDSPKey]*DSPStats)}
}

// SetKnobs wires live threshold resolution — called once at boot with a
// closure over the config handle, evaluated per selection so portal edits
// apply within the config poll interval, no restart.
func (r *SmartRouter) SetKnobs(fn func() Knobs) { r.knobsFn = fn }

func (r *SmartRouter) currentKnobs() Knobs {
	if r.knobsFn == nil {
		return DefaultKnobs()
	}
	return r.knobsFn()
}

// Seed pre-populates the router's per-(channel, DSP) stats from a prior
// aggregate — the warm-start from ClickHouse on boot (ADR 0003). Routing then
// survives a pod restart with real history instead of re-learning from cold.
// Existing entries for a (channel, DSP) are replaced. Intended to be called
// once at startup, before the handler serves traffic. WinRate isn't derivable
// from dsp_calls alone, so it's left at whatever the caller set (typically 0)
// and re-learned live from RecordWin.
func (r *SmartRouter) Seed(stats []DSPStats) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range stats {
		s := stats[i]
		r.stats[channelDSPKey{channel: s.Channel, dspID: s.DSPID}] = &s
	}
}

// Reseed replaces per-(channel, DSP) call stats with a cluster-global
// aggregate (reporting's dsp_calls, which every exchange replica feeds),
// PRESERVING locally-learned win history — wins aren't in dsp_calls, so
// overwriting them each reseed tick would zero WinRate forever. Entries
// absent from the aggregate keep their local state: a DSP whose calls
// haven't landed in analytics yet must not lose its fresh stats.
func (r *SmartRouter) Reseed(stats []DSPStats) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range stats {
		s := stats[i]
		key := channelDSPKey{channel: s.Channel, dspID: s.DSPID}
		if old, ok := r.stats[key]; ok {
			s.TotalWins = old.TotalWins
			if s.TotalBids > 0 {
				s.WinRate = float64(s.TotalWins) / float64(s.TotalBids)
			}
		}
		r.stats[key] = &s
	}
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
	if timedOut {
		s.TotalTimeouts++
	}

	// Rolling stats with an adaptive EWMA: α = 1/n until n reaches the
	// recency window (which IS the exact cumulative mean — warm-up behaviour
	// unchanged), then α = 1/window so the horizon stays ~window calls and a
	// DSP whose behaviour changes is re-judged on its recent self, not its
	// lifetime record. Totals stay cumulative (warm-up gate + observability).
	window := r.currentKnobs().RecencyWindow
	if window <= 0 {
		window = 200
	}
	alpha := 1 / float64(s.TotalCalls)
	if s.TotalCalls > window {
		alpha = 1 / float64(window)
	}
	bid := 0.0
	if bidReceived {
		bid = 1.0
		s.TotalBids++
		alphaBid := 1 / float64(s.TotalBids)
		if s.TotalBids > window {
			alphaBid = 1 / float64(window)
		}
		s.AvgBid += alphaBid * (bidPrice - s.AvgBid)
	}
	timeout := 0.0
	if timedOut {
		timeout = 1.0
	}
	s.BidRate += alpha * (bid - s.BidRate)
	s.TimeoutRate += alpha * (timeout - s.TimeoutRate)
	s.AvgLatency += time.Duration(alpha * float64(latency-s.AvgLatency))
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
// score so they aren't starved. No exploration probe (no trace to key it
// on) — the auction path uses SelectDSPsForTrace.
//
// Skip rules (per-channel, once TotalCalls exceeds Knobs.MinCalls; a DSP
// in Knobs.NeverSkip is exempt):
//   - bid_rate < Knobs.MinBidRate → almost never bids on this channel, skip
//   - timeout_rate > Knobs.MaxTimeoutRate → too slow on this channel, skip
//
// Ranking: expected value = bid_rate × avg_bid (× win_rate if any wins
// recorded), with a latency penalty (×0.8 above LatencySoft, ×0.5 above
// LatencyHard) so a fast bidder beats a slow bidder of equivalent EV.
//
// Knobs.Enabled=false bypasses everything and returns the input unchanged.
func (r *SmartRouter) SelectDSPs(channel string, allDSPs []string) []string {
	return r.selectDSPs(channel, allDSPs, "")
}

// SelectDSPsForTrace is SelectDSPs plus the ε-probe: a skip-filtered DSP is
// still included (appended after the ranked list) on Knobs.ExplorePct% of
// auctions, decided deterministically from (trace, dsp) so replays and
// tests are reproducible. This is what keeps a skipped DSP's stats flowing
// so it can earn its way back in when it recovers — without it, a skip is
// a lifetime sentence for the pod.
func (r *SmartRouter) SelectDSPsForTrace(channel string, allDSPs []string, traceID string) []string {
	return r.selectDSPs(channel, allDSPs, traceID)
}

func (r *SmartRouter) selectDSPs(channel string, allDSPs []string, traceID string) []string {
	k := r.currentKnobs()
	if !k.Enabled {
		out := make([]string, len(allDSPs))
		copy(out, allDSPs)
		return out
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	type scored struct {
		id    string
		score float64
	}

	var candidates []scored
	var explored []string
	for _, dspID := range allDSPs {
		key := channelDSPKey{channel: channel, dspID: dspID}
		s, ok := r.stats[key]
		if !ok {
			// No data yet for this (channel, dsp) — include with neutral
			// score so newly added DSPs and untested channels get a fair shot.
			candidates = append(candidates, scored{dspID, 0.5})
			continue
		}

		_, neverSkip := k.NeverSkip[dspID]
		if !neverSkip && s.TotalCalls > k.MinCalls {
			if s.BidRate < k.MinBidRate || s.TimeoutRate > k.MaxTimeoutRate {
				if traceID != "" && exploreTrace(traceID, dspID, k.ExplorePct) {
					explored = append(explored, dspID)
				}
				continue
			}
		}

		expectedValue := s.BidRate * s.AvgBid
		if s.WinRate > 0 {
			expectedValue *= s.WinRate
		}
		latencyPenalty := 1.0
		if s.AvgLatency > k.LatencySoft {
			latencyPenalty = 0.8
		}
		if s.AvgLatency > k.LatencyHard {
			latencyPenalty = 0.5
		}

		candidates = append(candidates, scored{dspID, expectedValue * latencyPenalty})
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	result := make([]string, 0, len(candidates)+len(explored))
	for _, c := range candidates {
		result = append(result, c.id)
	}
	// Explore probes ride at the back: they're being measured, not trusted.
	return append(result, explored...)
}

// exploreTrace deterministically decides whether this (trace, dsp) pair is
// in the exploration sample for pct (0-100). Same inputs → same decision,
// so a replayed trace explores identically and tests are reproducible.
func exploreTrace(traceID, dspID string, pct float64) bool {
	if pct <= 0 {
		return false
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(traceID))
	_, _ = h.Write([]byte{'|'})
	_, _ = h.Write([]byte(dspID))
	return float64(h.Sum32()%10000)/100.0 < pct
}

// Preview returns the list of DSPs SelectDSPs would currently return for
// (channel, allDSPs). Read-only — does not record a call, and shows the
// steady-state decision (no exploration probe). Used by the
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
