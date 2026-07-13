package billing

import (
	"context"
	"sync"
)

// CommittedCounter is a SHARED, cross-replica store of per-campaign committed
// spend (settled + open reserves) in micro-dollars, keyed by UTC day. It exists
// so the reporting service can run more than one replica.
//
// The problem it solves: a single reporting pod's in-memory pacingAccumulator
// (pacing.go) sees every event, so its snapshot is the whole truth. Run N pods
// behind a load-balanced NATS pull consumer and each sees only ~1/N of events —
// so each pod's in-memory view is a PARTIAL total. Publishing those partials as
// absolute snapshots makes them clobber each other on the DSP's budget key
// (last-writer-wins → under-count → overspend).
//
// The fix is additive + reconcile:
//   - Additive: every pod calls AddDelta with the SIGNED committed change from
//     the events it just billed. Redis HINCRBY (or an equivalent atomic add) is
//     commutative, so N pods' partial deltas sum to the correct total regardless
//     of how the load balancer split the stream — no single pod needs the whole
//     picture.
//   - Reconcile: additive counters drift (a lost delta under-counts → overspend;
//     a double-applied delta over-counts → under-delivery), because no single
//     message carries the whole truth to self-correct. So Reconcile periodically
//     re-asserts the authoritative per-campaign total recomputed from the
//     analytics store (the "sum all the data" pass), sweeping any drift.
//
// When no counter is wired (single replica, or Redis unreachable), the engine
// falls back to the in-memory pacingAccumulator snapshot and behaves exactly as
// before — this is a pure opt-in overlay.
type CommittedCounter interface {
	// AddDelta atomically applies signed micro-dollar deltas to the per-campaign
	// committed totals for the given UTC day. Deltas may be negative (a settle
	// releasing a larger reserve, or an expired reserve being swept).
	AddDelta(ctx context.Context, day string, deltas map[string]int64) error
	// Snapshot returns the current committed micro-dollars per campaign for the
	// day — the combined total across every replica that has called AddDelta.
	Snapshot(ctx context.Context, day string) (map[string]int64, error)
	// Reconcile overwrites the per-campaign committed totals for the day to the
	// authoritative values recomputed from the analytics store. Campaigns absent
	// from totals are left untouched (they had no store-visible activity; their
	// additive value stands until the daily key expires).
	Reconcile(ctx context.Context, day string, totals map[string]int64) error
}

// MemoryCommittedCounter is an in-process CommittedCounter for tests and for
// single-process fallback. It is safe for concurrent use, so a test can drive
// two Engine instances (simulating two reporting pods) sharing one counter and
// assert their combined view equals a single engine's — the core multi-replica
// correctness property.
type MemoryCommittedCounter struct {
	mu   sync.Mutex
	days map[string]map[string]int64 // day -> campaign -> micros
}

// NewMemoryCommittedCounter returns an empty in-memory counter.
func NewMemoryCommittedCounter() *MemoryCommittedCounter {
	return &MemoryCommittedCounter{days: make(map[string]map[string]int64)}
}

func (m *MemoryCommittedCounter) dayLocked(day string) map[string]int64 {
	d := m.days[day]
	if d == nil {
		d = make(map[string]int64)
		m.days[day] = d
	}
	return d
}

// AddDelta applies signed deltas under a single lock.
func (m *MemoryCommittedCounter) AddDelta(_ context.Context, day string, deltas map[string]int64) error {
	if len(deltas) == 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.dayLocked(day)
	for cid, delta := range deltas {
		if cid == "" || delta == 0 {
			continue
		}
		d[cid] += delta
	}
	return nil
}

// Snapshot returns a copy of the day's per-campaign totals.
func (m *MemoryCommittedCounter) Snapshot(_ context.Context, day string) (map[string]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	src := m.days[day]
	out := make(map[string]int64, len(src))
	for cid, v := range src {
		out[cid] = v
	}
	return out, nil
}

// Reconcile overwrites totals for the campaigns present in totals.
func (m *MemoryCommittedCounter) Reconcile(_ context.Context, day string, totals map[string]int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.dayLocked(day)
	for cid, v := range totals {
		if cid == "" {
			continue
		}
		d[cid] = v
	}
	return nil
}
