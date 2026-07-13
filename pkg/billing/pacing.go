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
	settledMicros int64
	holds         map[string]pacingHold // keyed by traceID
}

type pacingHold struct {
	micros  int64
	created time.Time
}

func newPacingAccumulator(clk clock.Clock) *pacingAccumulator {
	return &pacingAccumulator{
		clk:       clk,
		holdTTL:   defaultPacingHoldTTL,
		campaigns: make(map[string]*campaignPacing),
	}
}

// toMicros converts a dollar amount to integer micro-dollars (1 USD =
// 1,000,000 µ). Micros — not cents — because a realized per-impression cost is
// sub-cent: a $5.00 CPM books $0.005 = 5,000 µ, which cents (int64(0.5)) would
// truncate to zero. All the committed-spend counters are keyed in micros.
func toMicros(amount float64) int64 { return int64(math.Round(amount * 1_000_000)) }

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

// reset clears all accumulated pacing state. Used by the reporting debug
// billing-reset endpoint so e2e billing tests start from an empty committed
// view (the accumulator is otherwise long-lived + hydrated on boot).
func (p *pacingAccumulator) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.campaigns = make(map[string]*campaignPacing)
	p.day = ""
}

// recordBilled adds an immediately-billed (CPM) spend to today's settled total.
// Returns the committed-micros delta so the engine can mirror it to the shared
// CommittedCounter (billed spend raises committed by the full amount).
func (p *pacingAccumulator) recordBilled(campaignID string, amount float64) int64 {
	if campaignID == "" || amount <= 0 {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rollLocked()
	m := toMicros(amount)
	p.campaignLocked(campaignID).settledMicros += m
	return m
}

// recordReserve opens a hold for an impression awaiting its settle event. The
// hold counts toward committed until it settles or is swept. Returns the
// committed-micros delta (net of any prior hold on the same trace, so a
// redelivered reserve doesn't double-count).
func (p *pacingAccumulator) recordReserve(campaignID, traceID string, amount float64) int64 {
	if campaignID == "" || traceID == "" || amount <= 0 {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rollLocked()
	m := toMicros(amount)
	cp := p.campaignLocked(campaignID)
	old := cp.holds[traceID].micros
	cp.holds[traceID] = pacingHold{micros: m, created: p.clk.Now()}
	return m - old
}

// recordSettle converts an open hold into settled spend. Returns the
// committed-micros delta: the settled amount minus the hold it replaces (usually
// ~net-zero, since the hold already counted toward committed). If the hold is
// gone (swept, or the reserve landed on a prior day, or it settled on another
// replica) removed is zero and the settle raises committed by its full amount —
// still correct, because a settle means the spend was realized.
func (p *pacingAccumulator) recordSettle(campaignID, traceID string, amount float64) int64 {
	if campaignID == "" || amount <= 0 {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rollLocked()
	cp := p.campaignLocked(campaignID)
	removed := cp.holds[traceID].micros
	delete(cp.holds, traceID)
	m := toMicros(amount)
	cp.settledMicros += m
	return m - removed
}

// pacingKind selects which accumulator mutation a batch item applies.
type pacingKind int

const (
	pacingBilled  pacingKind = iota // CPM immediate → settled
	pacingReserve                   // CPC/CPA/vCPM/CPCV impression → open hold
	pacingSettle                    // CPC/CPA/vCPM/CPCV trigger → close hold, realize spend
)

// pacingItem is one campaign's contribution to a batched pacing update.
type pacingItem struct {
	campaignID string
	traceID    string // reserve holds are keyed by trace
	amount     float64
	kind       pacingKind
}

// recordBatch applies all items under a SINGLE lock (one rollLocked), removing
// the N lock acquisitions a per-event loop would take. Semantics are identical
// to calling recordBilled / recordReserve for each item in order.
// Returns the per-campaign committed-micros delta for the whole batch so the
// engine can mirror it to the shared CommittedCounter in one AddDelta call.
func (p *pacingAccumulator) recordBatch(items []pacingItem) map[string]int64 {
	if len(items) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rollLocked()
	now := p.clk.Now()
	deltas := make(map[string]int64)
	for _, it := range items {
		if it.campaignID == "" || it.amount <= 0 {
			continue
		}
		cp := p.campaignLocked(it.campaignID)
		switch it.kind {
		case pacingBilled:
			m := toMicros(it.amount)
			cp.settledMicros += m
			deltas[it.campaignID] += m
		case pacingReserve:
			if it.traceID == "" {
				continue
			}
			m := toMicros(it.amount)
			old := cp.holds[it.traceID].micros
			cp.holds[it.traceID] = pacingHold{micros: m, created: now}
			deltas[it.campaignID] += m - old
		case pacingSettle:
			// Close the open hold and realize the spend — net delta is the
			// settled amount minus the hold it replaces (usually ~zero, since
			// the hold already counted toward committed). Mirrors recordSettle.
			m := toMicros(it.amount)
			removed := cp.holds[it.traceID].micros
			delete(cp.holds, it.traceID)
			cp.settledMicros += m
			deltas[it.campaignID] += m - removed
		}
	}
	return deltas
}

// sweepExpired releases holds older than holdTTL and returns the number
// released plus the per-campaign committed-micros delta (negative — freeing a
// hold lowers committed). An expired hold is an impression whose billable settle
// event never arrived; freeing it keeps pacing from permanently over-counting.
func (p *pacingAccumulator) sweepExpired() (int, map[string]int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rollLocked()
	now := p.clk.Now()
	released := 0
	deltas := make(map[string]int64)
	for id, cp := range p.campaigns {
		for tid, h := range cp.holds {
			if now.Sub(h.created) >= p.holdTTL {
				delete(cp.holds, tid)
				released++
				deltas[id] -= h.micros
			}
		}
	}
	return released, deltas
}

// hydratedHoldKey is the trace-id slot used for the single synthetic hold that
// restores a campaign's aggregate open reserves on boot. Distinct from any real
// trace so a real reserve/settle never collides with it.
const hydratedHoldKey = "__hydrated__"

// pacingState returns the UTC day plus the persistable portions per campaign:
// settled micros (realized) and the aggregate open-reserved micros (sum of holds).
// Both are persisted so a restart can re-hydrate — settled resumes the day's
// realized spend, reserved restores in-flight holds so committed doesn't drop
// (which would reconcile DSP counters down and risk overspend).
func (p *pacingAccumulator) pacingState() (string, map[string]int64, map[string]int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rollLocked()
	settled := make(map[string]int64)
	reserved := make(map[string]int64)
	for id, cp := range p.campaigns {
		if cp.settledMicros > 0 {
			settled[id] = cp.settledMicros
		}
		var r int64
		for _, h := range cp.holds {
			r += h.micros
		}
		if r > 0 {
			reserved[id] = r
		}
	}
	return p.day, settled, reserved
}

// hydrate seeds settled totals and restores open reserves from durable storage
// on boot, so a restart resumes the day's committed spend instead of resetting
// to zero. Reserved is restored as ONE synthetic per-campaign hold, freshly
// dated so the sweep gives it a full TTL. Only applies when day matches the
// current UTC day (a stale previous-day load is ignored). Runs before event
// consumption, so it sets rather than merges.
//
// Caveat: a pre-restart reserve whose settle arrives after boot adds to settled
// while the synthetic hold still counts it — a bounded, conservative over-count
// (under-delivery, never overspend) that the sweep clears within one hold TTL.
func (p *pacingAccumulator) hydrate(day string, settled, reserved map[string]int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rollLocked()
	if day != p.day {
		return
	}
	now := p.clk.Now()
	for id, cents := range settled {
		if cents > 0 {
			p.campaignLocked(id).settledMicros = cents
		}
	}
	for id, cents := range reserved {
		if cents > 0 {
			p.campaignLocked(id).holds[hydratedHoldKey] = pacingHold{micros: cents, created: now}
		}
	}
}

// snapshot returns committed micro-dollars (settled + open holds) per campaign for
// today, for every campaign TOUCHED today — including those now at zero. A
// campaign whose only activity was reserves that all expired drops to zero, and
// it must still appear so the DSP reconciles its counter DOWN (otherwise the
// counter stays stuck at the last non-zero value until the daily key expires).
// Campaigns never touched today are absent from the map (not iterated), so the
// DSP leaves their local in-flight win counters alone. Safe to call
// concurrently with the record* methods.
func (p *pacingAccumulator) snapshot() map[string]int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rollLocked()
	out := make(map[string]int64, len(p.campaigns))
	for id, cp := range p.campaigns {
		committed := cp.settledMicros
		for _, h := range cp.holds {
			committed += h.micros
		}
		out[id] = committed
	}
	return out
}
