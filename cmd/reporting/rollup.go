package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/rollup"
)

// newRollupEngine builds the rollup engine and registers the standard
// configs. Always built (cheap) so the /debug/rollup/run trigger works
// even when the background scheduler is disabled.
func newRollupEngine(store analytics.Store, clk clock.Clock, log *slog.Logger) *rollup.Engine {
	engine := rollup.NewEngine(store, clk, log)
	engine.Register(rollup.EventsConfig)
	engine.Register(rollup.AuctionsConfig)
	return engine
}

// startRollupScheduler runs the rollup chain on a wall-clock cadence when
// reporting.rollup_enabled is true. A single minute ticker drives it: every
// minute rolls up the last completed minute; on the hour it also rolls up
// hourly; at UTC midnight, daily; on the 1st of the month, monthly. Off by
// default so dev/CI/e2e see no background activity unless explicitly
// enabled (this is the prod-only "rollup ownership lives here" switch).
func startRollupScheduler(engine *rollup.Engine, cfg *config.Config, clk clock.Clock, log *slog.Logger, lc *lifecycle.Lifecycle) {
	if !keys.Reporting.RollupEnabled.Get(cfg) {
		log.Info("rollup scheduler disabled (reporting.rollup_enabled=false)")
		return
	}
	stop := make(chan struct{})
	lc.OnShutdown("rollup-scheduler", func(_ context.Context) error {
		close(stop)
		return nil
	})
	go func() {
		ticker := clk.NewTicker(time.Minute)
		defer ticker.Stop()
		log.Info("rollup scheduler started", "cadence", "1m")
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				now := clk.Now().UTC()
				runRollup(engine, log, rollup.Minute)
				if now.Minute() == 0 {
					runRollup(engine, log, rollup.Hourly)
				}
				if now.Hour() == 0 && now.Minute() == 0 {
					runRollup(engine, log, rollup.Daily)
				}
				if now.Day() == 1 && now.Hour() == 0 && now.Minute() == 0 {
					runRollup(engine, log, rollup.Monthly)
				}
			}
		}
	}()
}

func runRollup(engine *rollup.Engine, log *slog.Logger, level rollup.Level) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := engine.RunLevel(ctx, level); err != nil {
		log.Error("rollup run failed", "level", string(level), "error", err)
	}
}

// rollupRunHandler triggers a synchronous rollup run for ?level=minute|
// hourly|daily|monthly (default minute) and returns the per-config results.
// Registration is gated by debug.endpoints_enabled.
func rollupRunHandler(engine *rollup.Engine, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		level := rollup.Level(strings.ToLower(r.URL.Query().Get("level")))
		switch level {
		case rollup.Minute, rollup.Hourly, rollup.Daily, rollup.Monthly:
		case "":
			level = rollup.Minute
		default:
			http.Error(w, `{"error":"level must be minute|hourly|daily|monthly"}`, http.StatusBadRequest)
			return
		}
		lookback := 1
		if lb := r.URL.Query().Get("lookback"); lb != "" {
			parsed, err := strconv.Atoi(lb)
			if err != nil || parsed < 1 || parsed > 1440 {
				http.Error(w, `{"error":"lookback must be 1..1440"}`, http.StatusBadRequest)
				return
			}
			lookback = parsed
		}
		// Budget scales with the window count (a lookback=60 minute run is
		// 60 windows × configs).
		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(30+lookback)*time.Second)
		defer cancel()
		results, err := engine.RunLevelLookback(ctx, level, lookback)
		if err != nil {
			log.Error("debug rollup run failed", "level", string(level), "error", err)
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(results)
	}
}
