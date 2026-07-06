package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/billing"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
)

// startSpendSnapshotPublisher periodically broadcasts the billing engine's
// per-campaign committed spend (settled-today + open reserves) so every DSP
// pod can reconcile its pacing budget counter to billed reality instead of the
// raw win-notice decrement it maintains locally. Each tick also sweeps expired
// reserves — impressions whose billable click/conversion/view never arrived —
// so a never-settled hold doesn't pace a campaign forever.
//
// Mirrors startRollupScheduler: config-gated, single ticker, clean shutdown via
// a stop channel + lifecycle hook. Runs on every reporting replica; the
// snapshot is idempotent (a full authoritative overwrite, not a delta), so
// multiple publishers just refresh the same numbers.
func startSpendSnapshotPublisher(engine *billing.Engine, bus events.EventBus, cfg *config.Config, clk clock.Clock, log *slog.Logger, lc *lifecycle.Lifecycle) {
	if !cfg.GetBool("reporting.spend_snapshot_enabled", true) {
		log.Info("spend snapshot publisher disabled (reporting.spend_snapshot_enabled=false)")
		return
	}
	interval := cfg.GetDuration("reporting.spend_snapshot_interval", 30*time.Second)
	if ttl := cfg.GetDuration("reporting.pacing_hold_ttl", 0); ttl > 0 {
		engine.SetPacingHoldTTL(ttl)
	}
	pub := events.NewPublisher(bus, log)

	stop := make(chan struct{})
	lc.OnShutdown("spend-snapshot-publisher", func(_ context.Context) error {
		close(stop)
		return nil
	})
	go func() {
		ticker := clk.NewTicker(interval)
		defer ticker.Stop()
		log.Info("spend snapshot publisher started", "interval", interval.String())
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				publishSpendSnapshot(engine, pub, clk, log)
			}
		}
	}()
}

func publishSpendSnapshot(engine *billing.Engine, pub *events.Publisher, clk clock.Clock, log *slog.Logger) {
	if released := engine.SweepExpiredHolds(); released > 0 {
		log.Info("swept expired pacing reserves", "released", released)
	}
	committed := engine.SnapshotCommitted()
	if len(committed) == 0 {
		return // nothing billed today yet — no DSP counter to correct
	}
	now := clk.Now().UTC()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pub.CampaignSpendSnapshot(ctx, events.CampaignSpendSnapshotEvent{
		Day:       now.Format("2006-01-02"),
		Currency:  "USD",
		Committed: committed,
		Timestamp: now,
	}); err != nil {
		log.Error("publish spend snapshot failed", "campaigns", len(committed), "error", err)
		return
	}
	log.Debug("published spend snapshot", "campaigns", len(committed))
}
