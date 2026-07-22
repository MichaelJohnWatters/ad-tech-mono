package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
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

		// End hour: the previous full clock hour by default (?hour= overrides).
		// At :10 past 14:00 this is 13:00 → the export covers [13:00, 14:00),
		// i.e. the whole 1pm hour, complete and settled.
		endHour := time.Now().UTC().Add(-time.Hour).Truncate(time.Hour)
		if h := strings.TrimSpace(r.URL.Query().Get("hour")); h != "" {
			t, err := time.Parse(time.RFC3339, h)
			if err != nil {
				http.Error(w, "bad hour (want RFC3339): "+err.Error(), http.StatusBadRequest)
				return
			}
			endHour = t.UTC().Truncate(time.Hour)
		}
		// Lookback: re-export the last N completed hours (default 2), overwriting
		// idempotently. This closes the straggler gap — a very-late event landing
		// after its hour's first export is picked up by the next run's lookback —
		// without ever double-counting (per-hour overwrite). ?hours= overrides.
		hoursBack := 2
		if hs := strings.TrimSpace(r.URL.Query().Get("hours")); hs != "" {
			if n, err := strconv.Atoi(hs); err == nil && n >= 1 && n <= 168 {
				hoursBack = n
			}
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

		hours := map[string]int64{}
		var total int64
		for i := 0; i < hoursBack; i++ {
			h := endHour.Add(time.Duration(-i) * time.Hour)
			counts, err := exporter.ExportHourToParquet(r.Context(), ecfg, h)
			if err != nil {
				log.Error("parquet export failed", "hour", h.Format(time.RFC3339), "error", err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			var hourTotal int64
			for _, n := range counts {
				hourTotal += n
			}
			hours[h.Format(time.RFC3339)] = hourTotal
			total += hourTotal
		}
		log.Info("parquet export complete", "end_hour", endHour.Format(time.RFC3339), "hours_back", hoursBack, "rows", total)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"end_hour": endHour.Format(time.RFC3339),
			"hours":    hours,
			"rows":     total,
		})
	}
}

// exportSnapshotHandler reports total exported row count per table across the
// Parquet export (ADR 0006 phase 5) — the reconciliation that replaced the
// retired Delta /debug/datalake/snapshot. Emits the same {table:{total_rows:n}}
// shape so the staff Batch/lake view and the e2e slippage check only change URL.
func exportSnapshotHandler(store analytics.Store, cfg *config.Config, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		exporter := unwrapExporter(store)
		w.Header().Set("Content-Type", "application/json")
		if exporter == nil {
			_ = json.NewEncoder(w).Encode(map[string]any{})
			return
		}
		endpoint := strings.TrimSpace(cfg.Get(keys.S3.Endpoint.Key(), ""))
		if endpoint == "" {
			http.Error(w, "s3.endpoint empty", http.StatusServiceUnavailable)
			return
		}
		counts, err := exporter.ExportSnapshot(r.Context(), analytics.ExportConfig{
			Endpoint:  endpoint,
			Bucket:    keys.Pipeline.DatalakeBucket.Get(cfg),
			AccessKey: keys.S3.AccessKey.Get(cfg),
			SecretKey: keys.S3.SecretKey.Get(cfg),
			UseSSL:    keys.S3.UseSSL.Get(cfg),
		})
		if err != nil {
			log.Error("export snapshot failed", "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out := make(map[string]map[string]int64, len(counts))
		for table, n := range counts {
			out[table] = map[string]int64{"total_rows": n}
		}
		_ = json.NewEncoder(w).Encode(out)
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
