package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache/warm"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
)

// startBidCacheRefresher is the background half of the "the bid path never
// touches Redis" contract: every tick it bulk-refreshes (one MGET each) the
// in-process copies the campaign loop reads per candidate — the budget spend
// counters and the balance draw-down mirrors. Both caches started life as
// inline-refill 1s TTL reads; the unlucky bid that hit expiry paid
// O(campaigns) serial Redis GETs (73 campaigns ≈ 66 round trips — the
// campaign_loop p95 tail, 2026-08-05). Now expiry doesn't exist: this loop
// owns freshness, the bid loop only reads process memory.
//
// The campaign warm cache is the source of the key set — campaign IDs for
// budget keys, deduped account IDs for balance keys — so a catalog change is
// picked up on the next tick. Runs for the life of the process (same posture
// as the taxonomy/identity preload loops); interval is read live each tick.
func startBidCacheRefresher(campaigns *warm.Cache[models.Campaign], budget *BudgetTracker, gate *BalanceGate, intervalFn func() time.Duration, log *slog.Logger) {
	if campaigns == nil || budget == nil {
		log.Warn("bid cache refresher not started (no campaign cache); bid-path spend reads will serve zeros")
		return
	}
	go func() {
		for {
			interval := intervalFn()
			if interval <= 0 {
				interval = time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), interval)
			all := campaigns.All()
			campaignIDs := make([]string, 0, len(all))
			accountSet := make(map[string]struct{}, len(all))
			accountIDs := make([]string, 0, len(all))
			for i := range all {
				campaignIDs = append(campaignIDs, all[i].ID)
				if id := all[i].AccountID; id != "" {
					if _, seen := accountSet[id]; !seen {
						accountSet[id] = struct{}{}
						accountIDs = append(accountIDs, id)
					}
				}
			}
			budget.RefreshSpend(ctx, campaignIDs)
			if gate != nil {
				gate.RefreshDeltas(ctx, accountIDs)
			}
			cancel()
			time.Sleep(interval)
		}
	}()
}
