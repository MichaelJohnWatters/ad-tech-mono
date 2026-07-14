package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/optimise"
)

// creativeStat mirrors analytics.CreativeStat's JSON — a local copy so the ad
// server doesn't import the analytics package (and its ClickHouse driver).
type creativeStat struct {
	CreativeID  string `json:"creative_id"`
	Impressions int    `json:"impressions"`
	Clicks      int    `json:"clicks"`
}

// warmStartBandit seeds the Thompson-sampling bandit from reporting's
// per-creative impressions/clicks so a restarted ad server doesn't reset every
// creative to a uniform prior (ADR 0003 part C). Boot-only, async, fail-open —
// never touches the serve hot path; SeedArm is pure in-memory. A cold bandit is
// no worse than before this existed.
func warmStartBandit(cfg *config.Config, bandit *optimise.Bandit, log *slog.Logger) {
	if !keys.AdServer.BanditWarmstart.Get(cfg) {
		return
	}
	base := keys.AdServer.ReportingURL.Get(cfg)
	url := base + "/debug/creative/stats?since_hours=24"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		log.Warn("bandit warm-start skipped: bad request", "error", err)
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Warn("bandit warm-start skipped: reporting unreachable", "url", url, "error", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Warn("bandit warm-start skipped: reporting non-200", "status", resp.StatusCode)
		return
	}
	var stats []creativeStat
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		log.Warn("bandit warm-start skipped: decode failed", "error", err)
		return
	}
	for _, s := range stats {
		bandit.SeedArm(s.CreativeID, s.Impressions, s.Clicks)
	}
	log.Info("bandit warm-started from reporting creative stats", "creatives", len(stats))
}
