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
	"encoding/json"
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

// routerResetMsg is the reset broadcast payload. Carrying the reset time
// makes the handler idempotent: JetStream redelivers on the ack-wait
// boundary, and a re-applied reset would otherwise wipe the router AGAIN
// mid-training and advance the reseed watermark past calls that already
// happened — exactly-once by timestamp comparison instead.
type routerResetMsg struct {
	AtMs int64 `json:"at_ms"`
}

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
		// Per-REPLICA name for broadcast semantics — same rule as the warm
		// caches: a shared group would load-balance the reset to one pod.
		// EPHEMERAL via SubscribeBroadcast (the c751a52 leak rule: a durable
		// consumer keyed by the ever-changing pod name is never reaped).
		name := "router-stats-" + podid.Replica()
		if err := rs.subscribeReset(name); err != nil {
			// Self-heal like the warm caches' resubscribeLoop: a NATS race at
			// boot must not permanently disable reset broadcasts for this pod
			// (observed live 2026-07-25: one Warn, then silent divergence).
			log.Warn("router reset subscribe failed, retrying in background", "error", err)
			go func() {
				for {
					time.Sleep(30 * time.Second)
					if rs.subscribeReset(name) == nil {
						rs.log.Info("router reset subscription established after retry")
						return
					}
				}
			}()
		}
	}
	go rs.reseedLoop()
	return rs
}

// subscribeReset attaches the broadcast handler for router-stats resets.
func (rs *routingSync) subscribeReset(name string) error {
	return events.SubscribeBroadcast(context.Background(), rs.bus, events.SubjectCacheInvalidateRouterStats, name,
		func(ctx context.Context, msg *events.Message) error {
			var m routerResetMsg
			_ = json.Unmarshal(msg.Data, &m)
			if m.AtMs == 0 {
				m.AtMs = time.Now().UnixMilli()
			}
			if rs.applyReset(m.AtMs) {
				rs.log.Info("smart router reset (broadcast)")
			}
			return nil
		})
}

// applyReset wipes the router and rebases the reseed watermark to atMs —
// but only if atMs is newer than the last applied reset, so a JetStream
// redelivery of the same broadcast is a no-op instead of a second wipe.
// Returns whether the reset was applied.
func (rs *routingSync) applyReset(atMs int64) bool {
	for {
		cur := rs.resetAt.Load()
		if atMs <= cur {
			return false
		}
		if rs.resetAt.CompareAndSwap(cur, atMs) {
			rs.router.Reset()
			return true
		}
	}
}

// broadcastReset resets locally and tells every other replica to do the
// same. Fail-open: if the publish fails the local reset still happened —
// same behaviour as before the broadcast existed.
func (rs *routingSync) broadcastReset(ctx context.Context) {
	at := time.Now().UnixMilli()
	rs.applyReset(at)
	if rs.bus == nil {
		return
	}
	payload, _ := json.Marshal(routerResetMsg{AtMs: at})
	if err := rs.bus.Publish(ctx, events.SubjectCacheInvalidateRouterStats, payload); err != nil {
		rs.log.Error("router reset broadcast failed; other replicas keep stale routing stats", "error", err)
	}
}

// reseedLoop converges this pod's router to the cluster-global dsp_calls
// aggregate. The interval is live-read each cycle so ops can retune or
// disable/re-enable from the portal; 0 pauses reseeding (per-pod stats +
// boot warm-start only) and is re-checked every 30s.
func (rs *routingSync) reseedLoop() {
	base := keys.Exchange.ReportingURL.Get(rs.cfg)
	for {
		interval := keys.Exchange.RoutingReseedInterval.Get(rs.cfg)
		if interval <= 0 {
			time.Sleep(30 * time.Second)
			continue
		}
		time.Sleep(interval)
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
