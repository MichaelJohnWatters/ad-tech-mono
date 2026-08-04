// Package preload is the DSP/SSP bid-path READER of the audience membership cache.
//
// Membership lives in Redis SETs (audience:set:{user}:{visibility}) maintained by
// the SINGLE append-based writer in cmd/pipeline: a trigger on
// audience_segment_members records every write to a change-log, and the writer
// applies atomic SADD/SREM (deltas) + an atomic ReplaceSet full reconcile. This
// package only READS those sets with SMEMBERS on the bid hot path — it no longer
// writes anything (the old per-pod preload/delta/invalidate machinery was retired
// when the read flipped to the append path). See docs/AUDIENCE_DATA_PATH_SCALING.md.
//
// Failure modes: Redis unreachable on read → empty ("no segments"), bid proceeds
// without enrichment, logged at debug.
package preload

import (
	"context"
	"database/sql"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache"
)

// Preloader reads the audience membership sets from Redis for the bid hot path.
type Preloader struct {
	l2       cache.L2Cache
	log      *slog.Logger
	lastLoad atomic.Int64
}

// Config wires a Preloader. DB/Interval/TTL are retained for call-site
// compatibility but unused now that the single pipeline writer owns writing.
type Config struct {
	DB       *sql.DB
	L2       cache.L2Cache
	Interval time.Duration
	TTL      time.Duration
	Log      *slog.Logger
}

// New constructs a read-only Preloader.
func New(cfg Config) *Preloader {
	return &Preloader{l2: cfg.L2, log: cfg.Log}
}

// Start marks the reader ready. There is nothing to preload — the pipeline writer
// populates Redis — so /readyz never blocks on a scan.
func (p *Preloader) Start(_ context.Context) error {
	p.lastLoad.Store(time.Now().UnixMilli())
	return nil
}

// Stop is a no-op (no background loop).
func (p *Preloader) Stop() {}

// Refresh is a no-op: the single pipeline writer owns cache freshness (e2e forces
// it via the pipeline's /debug/audience/refresh). Kept so the DSP/SSP debug
// endpoint still returns 200.
func (p *Preloader) Refresh(_ context.Context) error { return nil }

// LastLoaded reports when the reader became ready.
func (p *Preloader) LastLoaded() time.Time {
	ms := p.lastLoad.Load()
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// SegmentsForUser reads the user's public segments (SMEMBERS). Empty on a Redis
// miss = "no segments" — matches the caller's expectation for unknown users.
func (p *Preloader) SegmentsForUser(ctx context.Context, userID string) ([]string, error) {
	return p.lookup(ctx, userID, "public")
}

// DSPSegmentsForUser reads the user's dsp_private segments (SMEMBERS).
func (p *Preloader) DSPSegmentsForUser(ctx context.Context, userID string) ([]string, error) {
	return p.lookup(ctx, userID, "dsp_private")
}

func (p *Preloader) lookup(ctx context.Context, userID, visibility string) ([]string, error) {
	if userID == "" {
		return nil, nil
	}
	// A Redis blip degrades silently (empty = "no segments") — an error here would
	// WARN per bid and flood the logs.
	members, err := p.l2.SMembers(ctx, setRedisKey(userID, visibility))
	if err != nil {
		p.log.Debug("audience cache read failed", "user", userID, "visibility", visibility, "error", err)
		return nil, nil
	}
	return members, nil
}

// setRedisKey is the Redis SET key the append-based writer maintains and the bid
// path reads (must match cmd/pipeline's setKey).
func setRedisKey(userID, visibility string) string {
	return "audience:set:" + userID + ":" + visibility
}
