package main

// batch_monitor.go — staff view of batch-conductor chain runs (GET
// /v1/api/batch/runs, support:read): the last N runs with their per-step
// rows, newest first. The observability the old cron-offset lattice never
// had: what ran, in what order, what failed, what got skipped.

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
)

type batchStepView struct {
	Seq        int        `json:"seq"`
	Step       string     `json:"step"`
	Status     string     `json:"status"`
	Critical   bool       `json:"critical"`
	Detail     string     `json:"detail,omitempty"`
	Error      string     `json:"error,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

type batchRunView struct {
	RunID     string          `json:"run_id"`
	StartedAt time.Time       `json:"started_at"`
	Steps     []batchStepView `json:"steps"`
}

func batchMonitorHandler(db *sql.DB, log *slog.Logger) http.HandlerFunc {
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

		// Last 20 runs' steps in one query; group in-process.
		rows, err := db.QueryContext(r.Context(), `
SELECT run_id::text, seq, step, status, critical,
       COALESCE(detail,''), COALESCE(error,''), started_at, finished_at
FROM batch_runs
WHERE run_id IN (SELECT DISTINCT run_id FROM batch_runs ORDER BY run_id LIMIT 500)
  AND run_id IN (
      SELECT run_id FROM batch_runs GROUP BY run_id ORDER BY min(started_at) DESC LIMIT 20)
ORDER BY started_at DESC, run_id, seq`)
		if err != nil {
			log.Error("batch monitor query failed", "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		byRun := map[string]*batchRunView{}
		var runOrder []string
		for rows.Next() {
			var runID string
			var s batchStepView
			if err := rows.Scan(&runID, &s.Seq, &s.Step, &s.Status, &s.Critical,
				&s.Detail, &s.Error, &s.StartedAt, &s.FinishedAt); err != nil {
				continue
			}
			rv, ok := byRun[runID]
			if !ok {
				rv = &batchRunView{RunID: runID, StartedAt: s.StartedAt}
				byRun[runID] = rv
				runOrder = append(runOrder, runID)
			}
			if s.StartedAt.Before(rv.StartedAt) {
				rv.StartedAt = s.StartedAt
			}
			rv.Steps = append(rv.Steps, s)
		}
		out := make([]batchRunView, 0, len(runOrder))
		for _, id := range runOrder {
			rv := byRun[id]
			// steps arrive newest-first; re-sort by seq for display
			for i := 0; i < len(rv.Steps); i++ {
				for j := i + 1; j < len(rv.Steps); j++ {
					if rv.Steps[j].Seq < rv.Steps[i].Seq {
						rv.Steps[i], rv.Steps[j] = rv.Steps[j], rv.Steps[i]
					}
				}
			}
			out = append(out, *rv)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"runs": out})
	}
}
