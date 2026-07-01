package main

import (
	"log/slog"
	"os"
	"strings"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

// selectAnalyticsStore returns the analytics.Store implementation chosen
// by reporting.analytics_backend ("memory" or "duckdb").
//
// Memory is the default — dev/CI boot with zero infra, but it's volatile:
// every impression / click / conversion / view / auction-win is lost on
// pod restart. DuckDB is the durable local/staging story: an embedded
// columnar file, no server. ClickHouse (prod, multi-replica) is the
// eventual third option behind the same interface.
//
// DuckDB requires the `duckdb` build tag (the driver is CGO). A backend
// requested without that tag is a hard config error (os.Exit) rather than
// a silent fall-back to volatile memory — same posture as selectLedger:
// the operator either builds correctly or flips the backend explicitly.
//
// Note: the operational-signal events (freq-cap blocks, render failures,
// tracker rejections, budget depletions, serve no-fills, campaign state
// changes) and the /debug read-back endpoints are MemoryStore-only today.
// On the duckdb backend the consumer skips them (the type assertion in
// each handler fails) and the debug endpoints return 501 — core billing /
// analytics events still persist. Full parity is tracked in
// docs/MOCK_AUDIT.md (Phase A follow-up).
func selectAnalyticsStore(cfg *config.Config, log *slog.Logger) analytics.Store {
	backend := strings.ToLower(strings.TrimSpace(cfg.Get("reporting.analytics_backend", "memory")))
	switch backend {
	case "memory", "":
		log.Info("analytics store: memory backend (volatile — events lost on restart)")
		return analytics.NewMemory()
	case "duckdb":
		path := strings.TrimSpace(cfg.Get("reporting.duckdb_path", "/tmp/adtech-analytics.duckdb"))
		store, err := newDuckDBStore(path, log)
		if err != nil {
			log.Error("analytics store: duckdb backend requested but unavailable", "path", path, "error", err)
			os.Exit(1)
		}
		log.Info("analytics store: duckdb backend", "path", path)
		log.Warn("analytics store: /debug read-back endpoints are memory-only and return 501 on the duckdb backend (core + operational events are persisted); see docs/MOCK_AUDIT.md")
		return store
	case "clickhouse":
		addrs := splitAndTrim(cfg.Get("reporting.clickhouse_addr", "127.0.0.1:9000"))
		store, err := analytics.NewClickHouse(analytics.ClickHouseConfig{
			Addrs:    addrs,
			Database: cfg.Get("reporting.clickhouse_database", "adtech"),
			Username: cfg.Get("reporting.clickhouse_user", "adtech"),
			Password: cfg.Get("reporting.clickhouse_password", "adtech-local-dev"),
			Log:      log,
		})
		if err != nil {
			// The ledger's posture: a requested durable backend that's
			// unreachable is a config error, not a silent fall-back to
			// volatile memory (which would lose every event on restart).
			log.Error("analytics store: clickhouse backend requested but unreachable", "addrs", addrs, "error", err)
			os.Exit(1)
		}
		log.Info("analytics store: clickhouse backend", "addrs", addrs)
		log.Warn("analytics store: /debug read-back endpoints are memory-only and return 501 on the clickhouse backend (core + operational + rollup events are persisted); see docs/MOCK_AUDIT.md")
		return store
	default:
		log.Error("analytics store: unknown backend, refusing to boot",
			"reporting.analytics_backend", backend, "valid", []string{"memory", "duckdb", "clickhouse"})
		os.Exit(1)
		return nil
	}
}
