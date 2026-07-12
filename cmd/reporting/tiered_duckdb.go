//go:build duckdb

package main

import (
	"log/slog"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/datalake"
)

// maybeWrapTiered wraps the hot store in a TieredStore that serves deep history
// from the Parquet lake (DuckDB read_parquet over the active file set). Gated on reporting.tiered_enabled;
// only meaningful with the clickhouse backend (hot) — memory/duckdb hot stores
// are volatile/local and pair oddly with a cold lake, so we leave them hot-only.
// If the ParquetReader can't open (no S3 config, Minio down) we log and stay
// hot-only rather than fail boot.
func maybeWrapTiered(store analytics.Store, cfg *config.Config, log *slog.Logger) analytics.Store {
	if !cfg.GetBool("reporting.tiered_enabled", false) {
		return store
	}
	backend := strings.ToLower(strings.TrimSpace(cfg.Get("reporting.analytics_backend", "memory")))
	if backend != "clickhouse" {
		log.Warn("reporting.tiered_enabled ignored: cold tiering only applies to the clickhouse hot backend", "backend", backend)
		return store
	}

	endpoint := strings.TrimSpace(cfg.Get("s3.endpoint", ""))
	if endpoint == "" {
		log.Warn("reporting.tiered_enabled set but s3.endpoint is empty — serving hot-only (no lake to read)")
		return store
	}
	bucket := cfg.Get("pipeline.datalake_bucket", "adtech-datalake")
	accessKey := cfg.Get("s3.access_key", "adtech")
	secretKey := cfg.Get("s3.secret_key", "adtech-local-dev")
	region := cfg.Get("s3.region", "us-east-1")
	useSSL := cfg.GetBool("s3.use_ssl", false)

	reader, err := datalake.NewParquetReader(datalake.S3Config{
		Endpoint: endpoint, AccessKey: accessKey, SecretKey: secretKey, Region: region, UseSSL: useSSL,
	}, bucket)
	if err != nil {
		log.Error("reporting: cold tier reader failed to open — serving hot-only", "error", err)
		return store
	}

	hotWindow := cfg.GetDuration("reporting.hot_window", 7*24*time.Hour)
	cold := datalake.NewColdStore(reader)
	log.Info("reporting: hot/cold tiering enabled", "hot_window", hotWindow, "lake_bucket", bucket, "s3_endpoint", endpoint)
	return analytics.NewTieredStore(store, cold, hotWindow, log)
}
