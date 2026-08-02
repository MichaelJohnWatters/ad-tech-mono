package main

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache"
)

// FreqCap is the Redis-backed per-user-per-campaign impression counter.
//
// AllowAndRecord atomically INCRs the counter and reports whether the
// next impression is allowed. The first INCR also sets the TTL so each
// counter expires after the cap window.
//
// The limit + window are passed per call so the serve handler can supply the
// campaign's advertiser-configured cap (from the freq-cap warm cache) and fall
// back to the platform-default live-config values otherwise.
type FreqCap struct {
	l2  cache.L2Cache
	log *slog.Logger
}

func NewFreqCap(l2 cache.L2Cache, log *slog.Logger) *FreqCap {
	return &FreqCap{l2: l2, log: log}
}

func freqCapKey(userID, campaignID string) string {
	return "adserver:freqcap:" + userID + ":" + campaignID
}

// AllowAndRecord returns true if the impression is under the cap. Increments the
// counter and sets the window TTL on the first increment of a window. Empty
// userID (no consent) or a non-positive limit bypasses the cap entirely.
//
// This is the DISPLAY path: the ad server renders the ad here, so the serve
// decision is ~the impression and check-and-increment is correct. Video/audio
// are decoupled (async conditioning + server-side stitching in SSAI, player
// re-requests), so they PEEK here (Allow) and RECORD only when the ad is
// actually stitched — see the CapMode split in the serve handler.
func (f *FreqCap) AllowAndRecord(ctx context.Context, userID, campaignID string, limit int, window time.Duration) bool {
	if userID == "" || limit <= 0 {
		return true
	}
	key := freqCapKey(userID, campaignID)
	count, err := f.l2.Incr(ctx, key)
	if err != nil {
		f.log.Warn("freqcap incr failed", "key", key, "error", err)
		return true // fail-open: never block ads on cache failure
	}
	if count == 1 {
		if err := f.l2.Expire(ctx, key, window); err != nil {
			f.log.Warn("freqcap expire failed", "key", key, "error", err)
		}
	}
	return count <= int64(limit)
}

// Allow PEEKS the counter — reports whether the NEXT impression would be under
// the cap WITHOUT incrementing. Used by the serve decision for formats whose
// impression is confirmed later (video/audio: SSAI stitches + Records on fill),
// so a nobid or a cold conditioning-miss never burns a slot. The next
// impression is allowed when the current count is strictly below the limit
// (after a Record it becomes count+1 ≤ limit). Fail-open on cache error and on
// empty userID / non-positive limit (cap bypassed), matching AllowAndRecord.
func (f *FreqCap) Allow(ctx context.Context, userID, campaignID string, limit int) bool {
	if userID == "" || limit <= 0 {
		return true
	}
	key := freqCapKey(userID, campaignID)
	v, ok, err := f.l2.Get(ctx, key)
	if err != nil {
		f.log.Warn("freqcap peek failed", "key", key, "error", err)
		return true // fail-open
	}
	if !ok || v == "" {
		return true // no impressions yet this window
	}
	count, perr := strconv.ParseInt(v, 10, 64)
	if perr != nil {
		f.log.Warn("freqcap peek parse failed", "key", key, "value", v, "error", perr)
		return true // fail-open on a malformed counter
	}
	return count < int64(limit)
}

// Record INCREMENTs the counter (setting the window TTL on the first increment),
// with no allow/block decision — the caller already decided via Allow. Used to
// count a video/audio impression at stitch time, so the count reflects ads
// actually served, not serve decisions. No-op on empty userID / non-positive
// limit.
func (f *FreqCap) Record(ctx context.Context, userID, campaignID string, limit int, window time.Duration) {
	if userID == "" || limit <= 0 {
		return
	}
	key := freqCapKey(userID, campaignID)
	count, err := f.l2.Incr(ctx, key)
	if err != nil {
		f.log.Warn("freqcap record incr failed", "key", key, "error", err)
		return
	}
	if count == 1 {
		if err := f.l2.Expire(ctx, key, window); err != nil {
			f.log.Warn("freqcap record expire failed", "key", key, "error", err)
		}
	}
}
