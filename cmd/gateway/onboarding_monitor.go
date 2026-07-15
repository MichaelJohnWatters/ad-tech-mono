package main

// onboarding_monitor.go — the staff drop-zone monitor (profile store payoff
// valve): GET /v1/api/onboarding/runs lists recent onboarding_runs (written
// by the pipeline's drop-zone poller) plus per-provider rollups, so ops can
// see stuck providers, rejected-row spikes, and match-rate drift without
// grepping logs or Minio.
//
// Staff-only (support:read); platform-wide operational telemetry, same
// posture as the audit log.

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
)

type onboardingRunView struct {
	ID           string    `json:"id"`
	Provider     string    `json:"provider"`
	FileKey      string    `json:"file_key"`
	AccountID    string    `json:"account_id,omitempty"`
	SegmentID    string    `json:"segment_id,omitempty"`
	Status       string    `json:"status"`
	TotalRows    int       `json:"total_rows"`
	ValidRows    int       `json:"valid_rows"`
	RejectedRows int       `json:"rejected_rows"`
	MatchedRows  int       `json:"matched_rows"`
	MatchRate    *float64  `json:"match_rate,omitempty"`
	Error        string    `json:"error,omitempty"`
	RejectedKey  string    `json:"rejected_key,omitempty"`
	FinishedAt   time.Time `json:"finished_at"`
}

type onboardingProviderView struct {
	Provider     string    `json:"provider"`
	Files        int       `json:"files"`
	Failed       int       `json:"failed"`
	ValidRows    int       `json:"valid_rows"`
	RejectedRows int       `json:"rejected_rows"`
	AvgMatchRate *float64  `json:"avg_match_rate,omitempty"`
	LastRunAt    time.Time `json:"last_run_at"`
}

func onboardingMonitorHandler(db *sql.DB, log *slog.Logger) http.HandlerFunc {
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
		if db == nil {
			http.Error(w, `{"error":"store unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		ctx := r.Context()
		provider := strings.TrimSpace(r.URL.Query().Get("provider"))

		runs := []onboardingRunView{}
		rows, err := db.QueryContext(ctx, `
SELECT id::text, provider, file_key, COALESCE(account_id::text,''), COALESCE(segment_id::text,''),
       status, total_rows, valid_rows, rejected_rows, matched_rows, match_rate,
       COALESCE(error,''), COALESCE(rejected_key,''), finished_at
FROM onboarding_runs
WHERE ($1 = '' OR provider = $1)
ORDER BY finished_at DESC
LIMIT 100`, provider)
		if err != nil {
			log.Error("onboarding monitor: runs query failed", "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		for rows.Next() {
			var v onboardingRunView
			if err := rows.Scan(&v.ID, &v.Provider, &v.FileKey, &v.AccountID, &v.SegmentID,
				&v.Status, &v.TotalRows, &v.ValidRows, &v.RejectedRows, &v.MatchedRows,
				&v.MatchRate, &v.Error, &v.RejectedKey, &v.FinishedAt); err == nil {
				runs = append(runs, v)
			}
		}
		rows.Close()

		providers := []onboardingProviderView{}
		prows, err := db.QueryContext(ctx, `
SELECT provider, count(*),
       count(*) FILTER (WHERE status = 'failed'),
       COALESCE(sum(valid_rows),0), COALESCE(sum(rejected_rows),0),
       avg(match_rate), max(finished_at)
FROM onboarding_runs
GROUP BY provider
ORDER BY max(finished_at) DESC
LIMIT 50`)
		if err != nil {
			log.Error("onboarding monitor: provider rollup failed", "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		for prows.Next() {
			var v onboardingProviderView
			if err := prows.Scan(&v.Provider, &v.Files, &v.Failed, &v.ValidRows,
				&v.RejectedRows, &v.AvgMatchRate, &v.LastRunAt); err == nil {
				providers = append(providers, v)
			}
		}
		prows.Close()

		_ = json.NewEncoder(w).Encode(map[string]any{"runs": runs, "providers": providers})
	}
}
