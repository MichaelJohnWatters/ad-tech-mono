package main

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects"
)

// maybeWrapHotCold wraps the hot ClickHouse store in a HotColdStore that serves
// deep history (older than reporting.hot_window) from the Parquet export via
// ClickHouse's s3() table function (ADR 0006 phases 3-4). It's PURE GO — no
// `duckdb` build tag, no CGO delta_scan — because the cold reader is the same
// ClickHouse engine reading the export objects it wrote.
//
// Gated on reporting.cold_store_enabled and only meaningful with the clickhouse
// hot backend (memory/duckdb hot stores are volatile/local and pair oddly with a
// cold lake, so they stay hot-only). If the S3 config is missing or the cold
// reader can't connect, we log and serve hot-only rather than fail boot.
func maybeWrapHotCold(store analytics.Store, cfg *config.Config, log *slog.Logger) analytics.Store {
	if !keys.Reporting.ColdStoreEnabled.Get(cfg) {
		return store
	}
	backend := strings.ToLower(strings.TrimSpace(keys.Reporting.AnalyticsBackend.Get(cfg)))
	if backend != "clickhouse" {
		log.Warn("reporting.cold_store_enabled ignored: cold-store routing only applies to the clickhouse hot backend", "backend", backend)
		return store
	}

	endpoint := strings.TrimSpace(cfg.Get(keys.S3.Endpoint.Key(), ""))
	if endpoint == "" {
		log.Warn("reporting.cold_store_enabled set but s3.endpoint is empty — serving hot-only (no export to read)")
		return store
	}
	s3cfg := analytics.ExportConfig{
		Endpoint:  endpoint,
		Bucket:    keys.Pipeline.DatalakeBucket.Get(cfg),
		AccessKey: keys.S3.AccessKey.Get(cfg),
		SecretKey: keys.S3.SecretKey.Get(cfg),
		UseSSL:    keys.S3.UseSSL.Get(cfg),
	}
	chCfg := analytics.ClickHouseConfig{
		Addrs:    splitAndTrim(keys.Reporting.ClickHouseAddr.Get(cfg)),
		Database: keys.Reporting.ClickHouseDatabase.Get(cfg),
		Username: keys.Reporting.ClickHouseUser.Get(cfg),
		Password: keys.Reporting.ClickHousePassword.Get(cfg),
		Log:      log,
	}
	// Ensure the lake bucket exists: BOTH sides of the hot/cold spine go
	// through ClickHouse s3(), which cannot create buckets — on a fresh
	// object store every export errored NoSuchBucket and every summary/
	// rollup read paid a failing cold query until someone hand-made the
	// bucket (bitten twice: 2026-07-25 after the bucket vanished, and
	// 2026-07-26 on the factory-reset stack). Reporting owns the export,
	// so reporting owns the bucket. Fail-open: worst case is the same
	// degraded hot-only behaviour as before.
	if obj := objects.Connect(cfg, "", log); obj != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := obj.EnsureBucket(ctx, s3cfg.Bucket); err != nil {
			log.Error("reporting: lake bucket ensure failed — cold reads/exports will error until it exists",
				"bucket", s3cfg.Bucket, "error", err)
		}
		cancel()
	}

	cold, err := analytics.NewCHParquetColdReader(chCfg, s3cfg)
	if err != nil {
		log.Error("reporting: cold s3() reader failed to open — serving hot-only", "error", err)
		return store
	}

	hotWindow := keys.Reporting.HotWindow.Get(cfg)
	log.Info("reporting: hot/cold store enabled (clickhouse s3 cold reader)",
		"hot_window", hotWindow, "lake_bucket", s3cfg.Bucket, "s3_endpoint", endpoint)
	return analytics.NewHotColdStore(store, cold, hotWindow, log)
}
