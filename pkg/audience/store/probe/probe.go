// Package probe is Step 0 of the audience L1 cache: a SHADOW hit-rate probe.
//
// The per-auction audience lookup (SSP public + DSP dsp_private) is a Redis
// SMEMBERS on the bid hot path — the top steady I/O cost (~46ms p95, every
// auction). Before building an in-process L1 cache we need to know how often
// an L1 would actually hit, which depends on request locality (the same user
// firing N slot-auctions, session bursts) and is only knowable empirically.
//
// Probe wraps the real audience Lookup and, when enabled, counts would-be L1
// hits vs misses against a candidate TTL — WITHOUT changing behaviour (the
// inner lookup still runs on every call, so serving is byte-for-byte unchanged
// and it's safe to ship). Run it under load, read
// adtech_audience_l1_probe_{hits,misses}_total PER POD, and decide go/no-go.
//
// Cost discipline (it's on the hot path): disabled (the default) is a single
// atomic bool read and return — zero allocations, nothing else. Enabled is an
// O(1) sharded-map touch + a counter increment, with an inline alloc-free hash
// and no per-call string building. The map is bounded to the live working set
// by a background janitor, so it never grows with total users seen.
package probe

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	audstore "github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store"
)

const (
	numShards   = 256
	maxPerShard = 8192 // backstop cap (~2M keys/array); the real working set is rps×ttl, tiny
	janitorTick = 5 * time.Second
	defaultTTL  = 3 * time.Second
)

type shard struct {
	mu   sync.Mutex
	seen map[string]int64 // userID -> last-seen unix millis
}

// Probe decorates an audstore.Lookup with the shadow hit-rate probe. Public and
// dsp_private lookups are tracked in separate shard arrays so the map key is
// just the userID (no per-call concatenation).
type Probe struct {
	inner   audstore.Lookup
	enabled func() bool
	ttl     func() time.Duration

	pub  [numShards]shard
	priv [numShards]shard

	hitPublic, hitPrivate   prometheus.Counter
	missPublic, missPrivate prometheus.Counter

	stop chan struct{}
}

// Wrap returns a Probe over inner. enabled/ttl are live-config readers (so the
// probe can be toggled without a restart). Counters register on reg (the
// service's own registry); reg may be nil in tests.
func Wrap(inner audstore.Lookup, enabled func() bool, ttl func() time.Duration, reg prometheus.Registerer) *Probe {
	p := &Probe{inner: inner, enabled: enabled, ttl: ttl, stop: make(chan struct{})}
	for i := range p.pub {
		p.pub[i].seen = make(map[string]int64)
		p.priv[i].seen = make(map[string]int64)
	}
	hits := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "adtech", Subsystem: "audience", Name: "l1_probe_hits_total",
		Help: "Audience lookups that WOULD have been an L1 cache hit (same user within the probe TTL). Shadow only — serving unchanged.",
	}, []string{"visibility"})
	misses := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "adtech", Subsystem: "audience", Name: "l1_probe_misses_total",
		Help: "Audience lookups that would have MISSED the L1 cache (unseen user or beyond the probe TTL).",
	}, []string{"visibility"})
	if reg != nil {
		reg.MustRegister(hits, misses)
	}
	// Pre-bind the per-visibility counters so the hot path never does a label-map lookup.
	p.hitPublic, p.hitPrivate = hits.WithLabelValues("public"), hits.WithLabelValues("dsp_private")
	p.missPublic, p.missPrivate = misses.WithLabelValues("public"), misses.WithLabelValues("dsp_private")
	go p.janitor()
	return p
}

// SegmentsForUser records a would-be public lookup then delegates unchanged.
func (p *Probe) SegmentsForUser(ctx context.Context, userID string) ([]string, error) {
	p.record(&p.pub, userID, true)
	return p.inner.SegmentsForUser(ctx, userID)
}

// DSPSegmentsForUser records a would-be dsp_private lookup then delegates unchanged.
func (p *Probe) DSPSegmentsForUser(ctx context.Context, userID string) ([]string, error) {
	p.record(&p.priv, userID, false)
	return p.inner.DSPSegmentsForUser(ctx, userID)
}

func (p *Probe) record(shards *[numShards]shard, userID string, public bool) {
	// Disabled is the common case: one atomic read (LiveBool) + return.
	if userID == "" || !p.enabled() {
		return
	}
	ttlMs := p.ttlMillis()
	now := time.Now().UnixMilli()
	sh := &shards[shardIndex(userID)]
	sh.mu.Lock()
	last, ok := sh.seen[userID]
	hit := ok && now-last < ttlMs
	if ok || len(sh.seen) < maxPerShard {
		sh.seen[userID] = now
	}
	sh.mu.Unlock()
	switch {
	case hit && public:
		p.hitPublic.Inc()
	case hit:
		p.hitPrivate.Inc()
	case public:
		p.missPublic.Inc()
	default:
		p.missPrivate.Inc()
	}
}

func (p *Probe) ttlMillis() int64 {
	ttl := p.ttl()
	if ttl <= 0 {
		ttl = defaultTTL
	}
	return ttl.Milliseconds()
}

// shardIndex is an inline, allocation-free FNV-1a over the userID.
func shardIndex(key string) uint32 {
	const prime32 = 16777619
	h := uint32(2166136261)
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= prime32
	}
	return h % numShards
}

// janitor prunes entries older than the TTL so memory tracks the live working
// set (rps × ttl), not every user ever seen. Idle when the probe is disabled.
func (p *Probe) janitor() {
	t := time.NewTicker(janitorTick)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			if !p.enabled() {
				continue
			}
			cutoff := time.Now().UnixMilli() - p.ttlMillis()
			for i := range p.pub {
				prune(&p.pub[i], cutoff)
				prune(&p.priv[i], cutoff)
			}
		}
	}
}

func prune(sh *shard, cutoff int64) {
	sh.mu.Lock()
	for k, ts := range sh.seen {
		if ts < cutoff {
			delete(sh.seen, k)
		}
	}
	sh.mu.Unlock()
}

// Stop halts the janitor (call on service shutdown).
func (p *Probe) Stop() { close(p.stop) }
