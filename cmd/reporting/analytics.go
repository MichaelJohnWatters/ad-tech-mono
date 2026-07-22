package main

import (
	"log/slog"
	"os"
	"strings"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

// selectAnalyticsStore returns the analytics.Store implementation chosen
// by reporting.analytics_backend ("memory" or "clickhouse").
//
// Memory is the default — dev/CI boot with zero infra, but it's volatile:
// every impression / click / conversion / view / auction-win is lost on
// pod restart. ClickHouse is the durable store (real-time ingest + fast
// aggregation, pure-Go driver). DuckDB was retired as an analytics backend
// in ADR 0006 (ClickHouse is the single analytical store; the Parquet lake
// is a derived export read back via ClickHouse s3()).
//
// A requested-but-unreachable durable backend is a hard config error
// (os.Exit) rather than a silent fall-back to volatile memory — same posture
// as selectLedger: the operator either configures correctly or flips the
// backend explicitly.
//
// Note: the operational-signal events (freq-cap blocks, render failures,
// tracker rejections, budget depletions, serve no-fills, campaign state
// changes) and the /debug read-back endpoints are MemoryStore-only today.
// On the clickhouse backend the debug endpoints return 501 — core billing /
// analytics events still persist. Full parity is tracked in
// docs/PLAN.md -> "Build Status & Outstanding Work".
func selectAnalyticsStore(cfg *config.Config, log *slog.Logger) analytics.Store {
	backend := strings.ToLower(strings.TrimSpace(keys.Reporting.AnalyticsBackend.Get(cfg)))
	switch backend {
	case "memory", "":
		log.Info("analytics store: memory backend (volatile — events lost on restart)")
		return analytics.NewMemory()
	case "clickhouse":
		addrs := splitAndTrim(keys.Reporting.ClickHouseAddr.Get(cfg))
		// Bound the hot tier: TTL must comfortably EXCEED the cold-read
		// boundary (hot_window) so ClickHouse always covers what the cold
		// store hands off to it. If misconfigured smaller, clamp up to
		// hot_window + 1 day and warn — never let a TTL delete data the
		// hot path still owns.
		ttlDays := keys.Reporting.ClickHouseTTLDays.Get(cfg)
		if ttlDays > 0 {
			hotDays := int(keys.Reporting.HotWindow.Get(cfg).Hours()/24) + 1
			if ttlDays < hotDays {
				log.Warn("clickhouse TTL below hot_window; clamping up to keep the hot tier whole",
					"ttl_days", ttlDays, "hot_window_days", hotDays)
				ttlDays = hotDays
			}
		}
		store, err := analytics.NewClickHouse(analytics.ClickHouseConfig{
			Addrs:    addrs,
			Database: keys.Reporting.ClickHouseDatabase.Get(cfg),
			Username: keys.Reporting.ClickHouseUser.Get(cfg),
			Password: keys.Reporting.ClickHousePassword.Get(cfg),
			Log:      log,
			TTLDays:  ttlDays,
		})
		if err != nil {
			// The ledger's posture: a requested durable backend that's
			// unreachable is a config error, not a silent fall-back to
			// volatile memory (which would lose every event on restart).
			log.Error("analytics store: clickhouse backend requested but unreachable", "addrs", addrs, "error", err)
			os.Exit(1)
		}
		log.Info("analytics store: clickhouse backend", "addrs", addrs)
		log.Warn("analytics store: /debug read-back endpoints are memory-only and return 501 on the clickhouse backend (core + operational + rollup events are persisted); see docs/PLAN.md \"Build Status & Outstanding Work\"")
		return store
	default:
		log.Error("analytics store: unknown backend, refusing to boot",
			"reporting.analytics_backend", backend, "valid", []string{"memory", "clickhouse"})
		os.Exit(1)
		return nil
	}
}
