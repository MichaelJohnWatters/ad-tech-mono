package billing

import (
	"math"
	"sync"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
)

// defaultPacingHoldTTL bounds how long an open reserve (an impression that has
// reserved budget while awaiting its click/conversion/view settle) counts
// toward a campaign's committed spend before it is swept and released. A
// reserve that never settles is an impression whose billable event never
// arrived — for pacing that budget should be freed, matching the fact that
// CPC/CPA/vCPM only bill on the trigger event, not on the impression.
const defaultPacingHoldTTL = 15 * time.Minute

// pacingAccumulator tracks per-campaign COMMITTED spend for the current UTC
// day, where committed = settled-today + open-reserves. It is the
// authoritative pacing view the reporting service snapshots to the DSPs, so a
// DSP's budget gate reflects billed reality — phantom wins that never
// impressed are absent (they never billed), and reserves that never settle are
// swept — rather than the raw win prices the DSP counts locally.
//
// It is fed from the engine's billImmediate/reserve/settle paths, so it works
// identically whether the ledger backend is MemoryLedger or TigerBeetle.
// (TigerBeetle doesn't persist campaign_id, so an after-the-fact ledger query
// could not reconstruct this — the accumulator has to be built as events flow.)
//
// Campaign ids are the line-item UUIDs (idgen.Derive("line_item", ...)), the
// same id space the DSP's budget counter is keyed by, so a snapshot entry maps
// straight onto a DSP budget key.
type pacingAccumulator struct {
	mu        sync.Mutex
	clk       clock.Clock
	holdTTL   time.Duration
	day       string // UTC day the current campaigns map belongs to
	campaigns map[string]*campaignPacing
}

type campaignPacing struct {
	settledCents int64
	holds        map[string]pacingHold // keyed by traceID
}

type pacingHold struct {
	cents   int64
	created time.Time
}

func newPacingAccumulator(clk clock.Clock) *pacingAccumulator {
	return &pacingAccumulator{
		clk:       clk,
		holdTTL:   defaultPacingHoldTTL,
		campaigns: make(map[string]*campaignPacing),
	}
}

func toCents(amount float64) int64 { return int64(math.Round(amount * 100)) }

func dayKey(t time.Time) string { return t.UTC().Format("2006-01-02") }

// rollLocked resets the accumulator when the UTC day changes, so "committed"
// always means today (mirrors the DSP budget counter's daily TTL rollover).
// Caller must hold mu.
func (p *pacingAccumulator) rollLocked() {
	d := dayKey(p.clk.Now())
	if d != p.day {
		p.day = d
		p.campaigns = make(map[string]*campaignPacing)
	}
}

// campaignLocked returns (creating if needed) the pacing state for a campaign.
// Caller must hold mu.
func (p *pacingAccumulator) campaignLocked(campaignID string) *campaignPacing {
	cp := p.campaigns[campaignID]
	if cp == nil {
		cp = &campaignPacing{holds: make(map[string]pacingHold)}
		p.campaigns[campaignID] = cp
	}
	return cp
}

// recordBilled adds an immediately-billed (CPM) spend to today's settled total.
func (p *pacingAccumulator) recordBilled(campaignID string, amount float64) {
	if campaignID == "" || amount <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rollLocked()
	p.campaignLocked(campaignID).settledCents += toCents(amount)
}

// recordReserve opens a hold for an impression awaiting its settle event. The
// hold counts toward committed until it settles or is swept.
func (p *pacingAccumulator) recordReserve(campaignID, traceID string, amount float64) {
	if campaignID == "" || traceID == "" || amount <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rollLocked()
	p.campaignLocked(campaignID).holds[traceID] = pacingHold{cents: toCents(amount), created: p.clk.Now()}
}

// recordSettle converts an open hold into settled spend (net-neutral to
// committed: the hold was already counted). If the hold is gone (swept, or the
// reserve landed on a prior day) the settled amount is still counted, because a
// settle means the spend was realized.
func (p *pacingAccumulator) recordSettle(campaignID, traceID string, amount float64) {
	if campaignID == "" || amount <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rollLocked()
	cp := p.campaignLocked(campaignID)
	delete(cp.holds, traceID)
	cp.settledCents += toCents(amount)
}

// sweepExpired releases holds older than holdTTL and returns the number
// released. An expired hold is an impression whose billable settle event never
// arrived; freeing it keeps pacing from permanently over-counting.
func (p *pacingAccumulator) sweepExpired() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rollLocked()
	now := p.clk.Now()
	released := 0
	for _, cp := range p.campaigns {
		for tid, h := range cp.holds {
			if now.Sub(h.created) >= p.holdTTL {
				delete(cp.holds, tid)
				released++
			}
		}
	}
	return released
}

// settledSnapshot returns the durable (settled-today) portion per campaign,
// excluding transient open reserves, plus the UTC day it belongs to. This is
// what gets persisted so a restart can re-hydrate — holds are deliberately
// excluded because they rebuild from live reserves within the hold TTL.
func (p *pacingAccumulator) settledSnapshot() (string, map[string]int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rollLocked()
	out := make(map[string]int64, len(p.campaigns))
	for id, cp := range p.campaigns {
		if cp.settledCents > 0 {
			out[id] = cp.settledCents
		}
	}
	return p.day, out
}

// hydrateSettled seeds settled totals loaded from durable storage on boot, so a
// restart doesn't reset the day's committed spend to zero (which would reconcile
// DSP counters down and risk overspend). Only applies when day matches the
// current UTC day — a stale (previous-day) load is ignored. Intended to run
// before event consumption starts, so it sets rather than merges.
func (p *pacingAccumulator) hydrateSettled(day string, m map[string]int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rollLocked()
	if day != p.day {
		return
	}
	for id, cents := range m {
		if cents <= 0 {
			continue
		}
		p.campaignLocked(id).settledCents = cents
	}
}

// snapshot returns committed cents (settled + open holds) per campaign for
// today, skipping campaigns at zero. Safe to call concurrently with the
// record* methods.
func (p *pacingAccumulator) snapshot() map[string]int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rollLocked()
	out := make(map[string]int64, len(p.campaigns))
	for id, cp := range p.campaigns {
		committed := cp.settledCents
		for _, h := range cp.holds {
			committed += h.cents
		}
		if committed > 0 {
			out[id] = committed
		}
	}
	return out
}
