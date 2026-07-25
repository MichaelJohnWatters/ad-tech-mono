// Cross-replica SmartRouter state sync.
//
// Router stats are in-process, and at N replicas each pod sees only ~1/N of
// the fan-out calls — so no pod ever crosses exchange.routing_min_calls on
// its own, skip decisions diverge per pod, and a /debug reset only wipes
// the one pod the load balancer picked. Two mechanisms fix that, both
// fail-open and off the auction hot path:
//
//  1. Periodic reseed: every exchange.routing_reseed_interval each pod
//     refetches reporting's cluster-global dsp_calls aggregate (the same
//     source the boot warm-start uses — every pod's calls land there via
//     NATS → ClickHouse) and Reseed()s its router. All pods converge to
//     the shared totals; win history stays pod-local (not in dsp_calls).
//  2. Reset broadcast: the debug reset publishes a router-stats cache
//     invalidate; every pod resets its router AND rebases its reseed
//     watermark, so pre-reset history can't leak back in via the reseed.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/optimise"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/podid"
)

type routingSync struct {
	cfg    *config.Config
	router *optimise.SmartRouter
	bus    events.EventBus // nil = no NATS; resets stay pod-local
	log    *slog.Logger

	// resetAt is the unix-ms watermark of the last reset (local or
	// broadcast). The reseed only folds dsp_calls newer than this, so a
	// reset genuinely forgets history instead of re-importing it.
	resetAt atomic.Int64
}

// startRoutingSync wires the reset-broadcast subscription and the periodic
// reseed loop. Safe with bus == nil (poll-only, pod-local resets).
func startRoutingSync(cfg *config.Config, router *optimise.SmartRouter, bus events.EventBus, log *slog.Logger) *routingSync {
	rs := &routingSync{cfg: cfg, router: router, bus: bus, log: log}
	if bus != nil {
		// Per-REPLICA group for broadcast semantics — same rule as the warm
		// caches: a shared group would load-balance the reset to one pod.
		group := "router-stats-" + podid.Replica()
		err := bus.Subscribe(context.Background(), events.SubjectCacheInvalidateRouterStats, group,
			func(ctx context.Context, msg *events.Message) error {
				rs.resetLocal()
				rs.log.Info("smart router reset (broadcast)")
				return nil
			})
		if err != nil {
			log.Warn("router reset subscribe failed; resets reach this pod only via its own debug endpoint", "error", err)
		}
	}
	go rs.reseedLoop()
	return rs
}

// resetLocal wipes this pod's router and rebases the reseed watermark.
func (rs *routingSync) resetLocal() {
	rs.resetAt.Store(time.Now().UnixMilli())
	rs.router.Reset()
}

// broadcastReset resets locally and tells every other replica to do the
// same. Fail-open: if the publish fails the local reset still happened —
// same behaviour as before the broadcast existed.
func (rs *routingSync) broadcastReset(ctx context.Context) {
	rs.resetLocal()
	if rs.bus == nil {
		return
	}
	if err := rs.bus.Publish(ctx, events.SubjectCacheInvalidateRouterStats, []byte(`{}`)); err != nil {
		rs.log.Error("router reset broadcast failed; other replicas keep stale routing stats", "error", err)
	}
}

// reseedLoop converges this pod's router to the cluster-global dsp_calls
// aggregate. Interval 0 disables (per-pod stats + boot warm-start only).
func (rs *routingSync) reseedLoop() {
	interval := keys.Exchange.RoutingReseedInterval.Get(rs.cfg)
	if interval <= 0 {
		return
	}
	base := keys.Exchange.ReportingURL.Get(rs.cfg)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		url := base + "/debug/routing/stats?since_hours=6"
		if at := rs.resetAt.Load(); at > 0 {
			url = fmt.Sprintf("%s/debug/routing/stats?since_ms=%d", base, at)
		}
		seed, ok := fetchRoutingStats(url, rs.log, "routing reseed")
		if !ok {
			continue
		}
		rs.router.Reseed(seed)
	}
}
