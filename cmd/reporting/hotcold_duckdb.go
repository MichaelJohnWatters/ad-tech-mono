//go:build duckdb

package main

import (
	"log/slog"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/datalake"
)

// maybeWrapHotCold wraps the hot store in a HotColdStore that serves deep history
// from the Parquet lake (DuckDB read_parquet over the active file set). Gated on reporting.cold_store_enabled;
// only meaningful with the clickhouse backend (hot) — memory/duckdb hot stores
// are volatile/local and pair oddly with a cold lake, so we leave them hot-only.
// If the ParquetReader can't open (no S3 config, Minio down) we log and stay
// hot-only rather than fail boot.
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
		log.Warn("reporting.cold_store_enabled set but s3.endpoint is empty — serving hot-only (no lake to read)")
		return store
	}
	bucket := keys.Pipeline.DatalakeBucket.Get(cfg)
	accessKey := keys.S3.AccessKey.Get(cfg)
	secretKey := keys.S3.SecretKey.Get(cfg)
	region := keys.S3.Region.Get(cfg)
	useSSL := keys.S3.UseSSL.Get(cfg)

	reader, err := datalake.NewParquetReader(datalake.S3Config{
		Endpoint: endpoint, AccessKey: accessKey, SecretKey: secretKey, Region: region, UseSSL: useSSL,
	}, bucket)
	if err != nil {
		log.Error("reporting: cold store reader failed to open — serving hot-only", "error", err)
		return store
	}

	hotWindow := keys.Reporting.HotWindow.Get(cfg)
	cold := datalake.NewColdStore(reader)
	log.Info("reporting: hot/cold store enabled", "hot_window", hotWindow, "lake_bucket", bucket, "s3_endpoint", endpoint)
	return analytics.NewHotColdStore(store, cold, hotWindow, log)
}
