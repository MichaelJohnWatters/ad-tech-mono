package main

// batch_lake.go — staff lake-state view (GET /v1/api/batch/lake,
// support:read): proxies the pipeline's /debug/datalake/snapshot so the
// Batch runs page can show per-table rows / active file counts next to the
// chain that writes them. The troubleshooting loop this closes: "compact
// says 3 tables packed — did the file counts actually drop?" without
// kubectl or the Minio console.
import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
)

func batchLakeHandler(pipelineURL string, log *slog.Logger) http.HandlerFunc {
	client := &http.Client{Timeout: 60 * time.Second} // snapshot flushes first
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
		resp, err := client.Get(pipelineURL + "/debug/datalake/snapshot")
		if err != nil {
			log.Warn("batch lake snapshot: pipeline unreachable", "error", err)
			http.Error(w, `{"error":"pipeline unreachable"}`, http.StatusBadGateway)
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
