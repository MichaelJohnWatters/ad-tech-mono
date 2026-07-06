package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache/warm"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
)

// startPacingReconcile subscribes to the reporting service's per-campaign
// committed-spend snapshots and reconciles this pod's Redis budget counters to
// billed reality. The local win-notice Record path is a fast, conservative
// over-count (phantom wins, full clearing price on CPC/CPA); each snapshot
// snaps the counter back to what actually bills.
//
// FAN-OUT, not queue-grouped: DSP pods can be sharded by account, so every pod
// must see every snapshot and reconcile the campaigns IT owns (ByID hit).
// Campaigns this pod doesn't own are skipped — another pod owns them, and the
// Redis budget key is shared, so a redundant Set of the same value is harmless.
// The per-pod group (mirrors warm-cache invalidation) gives each pod its own
// durable consumer so all pods receive the broadcast.
func startPacingReconcile(ctx context.Context, bus events.EventBus, campaigns *warm.Cache[models.Campaign], budget *BudgetTracker, cfg *config.Config, log *slog.Logger) error {
	if bus == nil || campaigns == nil || budget == nil {
		return nil // nothing to reconcile against (YAML-only boot / no NATS)
	}
	if !cfg.GetBool("dsp.spend_reconcile_enabled", true) {
		log.Info("pacing spend reconcile disabled (dsp.spend_reconcile_enabled=false)")
		return nil
	}
	podID := os.Getenv("POD_NAME")
	if podID == "" {
		podID = fmt.Sprintf("pid-%d", os.Getpid())
	}
	group := "spend-reconcile-" + podID
	handler := func(_ context.Context, msg *events.Message) error {
		var ev events.CampaignSpendSnapshotEvent
		if err := json.Unmarshal(msg.Data, &ev); err != nil {
			log.Warn("dropping malformed spend snapshot", "error", err)
			return msg.Ack()
		}
		reconciled := 0
		for campaignID, cents := range ev.Committed {
			if _, ok := campaigns.ByID(campaignID); !ok {
				continue // not this pod's campaign
			}
			budget.Reconcile(ev.Day, campaignID, cents)
			reconciled++
		}
		if reconciled > 0 {
			log.Debug("pacing reconciled from snapshot", "day", ev.Day, "campaigns", reconciled, "in_snapshot", len(ev.Committed))
		}
		return msg.Ack()
	}
	if err := bus.Subscribe(ctx, events.SubjectCampaignSpendSnapshot, group, handler); err != nil {
		return fmt.Errorf("subscribe %s: %w", events.SubjectCampaignSpendSnapshot, err)
	}
	log.Info("pacing spend reconcile consuming snapshots", "subject", events.SubjectCampaignSpendSnapshot)
	return nil
}
