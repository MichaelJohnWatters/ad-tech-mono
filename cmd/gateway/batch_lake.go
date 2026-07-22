package main

// batch_lake.go — staff lake-state view (GET /v1/api/batch/lake,
// support:read): proxies reporting's /debug/export/snapshot so the Batch runs
// page can show per-table exported row counts next to the chain that derives
// them. Since ADR 0006 phase 5 the lake is a derived hourly ClickHouse→Parquet
// export (not a live Delta dual-write), so the source of truth for "did every
// event reach the archive?" is reporting, not the retired pipeline sink.
import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

func batchLakeHandler(reportingURL string, log *slog.Logger) http.HandlerFunc {
	client := &http.Client{Timeout: 60 * time.Second}
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		if !can(claims, "support:read") {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		resp, err := client.Get(reportingURL + routes.ReportingExportSnapshot)
		if err != nil {
			log.Warn("batch lake snapshot: reporting unreachable", "error", err)
			http.Error(w, `{"error":"reporting unreachable"}`, http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": string(body)})
			return
		}
		_, _ = io.Copy(w, resp.Body)
	}
}
