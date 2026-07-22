package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

// exportRunHandler snapshots the ClickHouse event + signal tables for one hour
// to Parquet on the lake bucket via s3() (ADR 0006 phase 4). Idempotent per
// hour (s3_truncate_on_insert). Only the clickhouse backend can export; on
// memory/duckdb it returns 200 with a skip note so the batch-conductor step
// stays green. Defaults to the previous full hour; ?hour=RFC3339 overrides.
func exportRunHandler(store analytics.Store, cfg *config.Config, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		exporter := unwrapExporter(store)
		if exporter == nil {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"skipped": "no parquet exporter (analytics backend is not clickhouse)"})
			return
		}

		hour := time.Now().UTC().Add(-time.Hour).Truncate(time.Hour)
		if h := strings.TrimSpace(r.URL.Query().Get("hour")); h != "" {
			t, err := time.Parse(time.RFC3339, h)
			if err != nil {
				http.Error(w, "bad hour (want RFC3339): "+err.Error(), http.StatusBadRequest)
				return
			}
			hour = t.UTC().Truncate(time.Hour)
		}

		endpoint := strings.TrimSpace(cfg.Get(keys.S3.Endpoint.Key(), ""))
		if endpoint == "" {
			http.Error(w, "s3.endpoint empty — cannot export", http.StatusServiceUnavailable)
			return
		}
		ecfg := analytics.ExportConfig{
			Endpoint:  endpoint,
			Bucket:    keys.Pipeline.DatalakeBucket.Get(cfg),
			AccessKey: keys.S3.AccessKey.Get(cfg),
			SecretKey: keys.S3.SecretKey.Get(cfg),
			UseSSL:    keys.S3.UseSSL.Get(cfg),
		}

		counts, err := exporter.ExportHourToParquet(r.Context(), ecfg, hour)
		if err != nil {
			log.Error("parquet export failed", "hour", hour, "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var total int64
		for _, n := range counts {
			total += n
		}
		log.Info("parquet export complete", "hour", hour.Format(time.RFC3339), "tables", len(counts), "rows", total)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"hour":   hour.Format(time.RFC3339),
			"rows":   total,
			"tables": counts,
		})
	}
}

// unwrapExporter finds the ParquetExporter (the ClickHouse store), unwrapping a
// HotColdStore — the cold wrapper never exports; the hot ClickHouse tables are
// the export source.
func unwrapExporter(store analytics.Store) analytics.ParquetExporter {
	if e, ok := store.(analytics.ParquetExporter); ok {
		return e
	}
	if hc, ok := store.(*analytics.HotColdStore); ok {
		if e, ok := hc.HotStore().(analytics.ParquetExporter); ok {
			return e
		}
	}
	return nil
}
