package main

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/podid"
)

// taxonomyCache is the SSP's L1 warm map of public segment id → IAB Audience
// Taxonomy node id (audience_segments.taxonomy_id, public visibility only).
// The bid-request hot path already knows which public segments a user is in;
// this map just translates those ids into standard taxonomy ids for the
// OpenRTB user.data stamp — a pure in-memory lookup, no per-request query.
//
// Same degradation stance as the segment lookup itself: if the refresh
// fails, the previous map keeps serving (or stays empty on a cold boot) and
// bid requests simply omit user.data — never stall the auction.
type taxonomyCache struct {
	src taxonomySource
	log *slog.Logger

	mu   sync.RWMutex
	byID map[string]int64
}

// taxonomySource is the store read the cache refreshes from — satisfied by
// *audiencepg.Store.
type taxonomySource interface {
	PublicSegmentTaxonomy(ctx context.Context) (map[string]int64, error)
}

// newTaxonomyCache starts the refresh loop and returns the cache. The first
// refresh is synchronous with a short deadline so a warm boot serves labels
// immediately; failures degrade to an empty map.
func newTaxonomyCache(ctx context.Context, src taxonomySource, interval time.Duration, log *slog.Logger) *taxonomyCache {
	c := &taxonomyCache{src: src, log: log, byID: map[string]int64{}}
	c.refresh(ctx)
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				c.refresh(ctx)
			}
		}
	}()
	return c
}

// refresh re-reads the map; nil-safe so the debug refresh path can call it
// unconditionally alongside the preloader.
func (c *taxonomyCache) refresh(ctx context.Context) {
	if c == nil {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	m, err := c.src.PublicSegmentTaxonomy(rctx)
	if err != nil {
		c.log.Warn("taxonomy map refresh failed (serving previous)", "error", err)
		return
	}
	c.mu.Lock()
	c.byID = m
	c.mu.Unlock()
}

// subscribeInvalidate refreshes the map on adtech.cache.invalidate.audience —
// the same subject membership writes publish, and what the gateway's
// taxonomy-label endpoint publishes. Per-REPLICA group (not POD_NAME) for
// broadcast semantics, same rationale as the preloader's SubscribeInvalidate;
// the group name carries the subject leaf + "taxonomy" so it can't collide
// with the preloader's consumer. Failure degrades to poll-only.
func (c *taxonomyCache) subscribeInvalidate(ctx context.Context, bus events.EventBus) {
	if c == nil || bus == nil {
		return
	}
	group := constants.ServiceSSP + "-audience-taxonomy-" + podid.Replica()
	err := bus.Subscribe(ctx, events.SubjectCacheInvalidateAudience, group, func(_ context.Context, msg *events.Message) error {
		c.refresh(ctx)
		_ = msg.Ack()
		return nil
	})
	if err != nil {
		c.log.Warn("taxonomy invalidate subscribe failed (poll-only)", "error", err)
		return
	}
	c.log.Info("taxonomy cache subscribed to audience invalidates", "group", group)
}

// dataSegments maps the request's public segment ids to OpenRTB data
// segments carrying their taxonomy node ids. Unlabelled segments are simply
// skipped — they stay platform-internal on user.ext.segments.
func (c *taxonomyCache) dataSegments(segmentIDs []string) []openrtb.DataSegment {
	if c == nil || len(segmentIDs) == 0 {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []openrtb.DataSegment
	seen := map[int64]bool{}
	for _, id := range segmentIDs {
		tid, ok := c.byID[id]
		if !ok || seen[tid] {
			continue
		}
		seen[tid] = true
		out = append(out, openrtb.DataSegment{ID: strconv.FormatInt(tid, 10)})
	}
	return out
}
