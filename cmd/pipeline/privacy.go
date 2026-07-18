package main

// privacy.go — the lake's GDPR purge surface.
//
// profile_signals and behaviour_signals are the only lake tables carrying
// user keys, and the lake has ONE writer (this process — the sink and the
// drop-zone poller share the ObjectStore's write lock). So the Level-3
// deletion pipeline (cmd/privacy-delete / privacy-verify) doesn't rewrite
// Delta files itself: it calls these endpoints and the purge runs in-process
// here, serialized with normal appends.
//
//	POST /v1/datalake/purge    {"user_id": "..."} → {"profile_signals": n, "behaviour_signals": m}
//	GET  /v1/datalake/residual ?user_id=...       → same shape, read-only counts

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/datalake"
)

// lakeUserTables maps each user-keyed lake table to its match predicate.
// profile_signals rows key on id_value (the onboarded identifier);
// behaviour_signals rows key on user_id or household_id.
func lakeUserTables(userID string) map[string]func(datalake.Record) bool {
	return map[string]func(datalake.Record) bool{
		profileSignalsTable: func(r datalake.Record) bool {
			return r["id_value"] == userID
		},
		behaviourSignalsTable: func(r datalake.Record) bool {
			return r["user_id"] == userID || r["household_id"] == userID
		},
	}
}

// PurgeUser removes every lake row keyed to userID. Buffered rows for the
// two tables are flushed first so a row sitting in the batch buffer can't
// land after the rewrite and resurrect the user.
func (s *datalakeSink) PurgeUser(ctx context.Context, userID string) (map[string]int, error) {
	out := map[string]int{}
	for table, match := range lakeUserTables(userID) {
		s.flushTable(ctx, table)
		n, err := s.lake.PurgeRows(ctx, table, match)
		if err != nil {
			return nil, fmt.Errorf("purge %s: %w", table, err)
		}
		out[table] = n
	}
	return out, nil
}

// ResidualUser counts lake rows still keyed to userID (0 across the board =
// clean). Flushes first for the same reason as PurgeUser.
func (s *datalakeSink) ResidualUser(ctx context.Context, userID string) (map[string]int, error) {
	out := map[string]int{}
	for table, match := range lakeUserTables(userID) {
		s.flushTable(ctx, table)
		n, err := s.lake.CountRows(ctx, table, match)
		if err != nil {
			return nil, fmt.Errorf("count %s: %w", table, err)
		}
		out[table] = n
	}
	return out, nil
}

// registerPrivacyEndpoints mounts the purge + residual handlers. Internal
// service surface (cluster network / privacy-delete job), not exposed via
// the gateway.
func registerPrivacyEndpoints(mux *http.ServeMux, sink *datalakeSink) {
	mux.HandleFunc("POST "+routes.DatalakePurge, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		var req struct {
			UserID string `json:"user_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.UserID == "" {
			http.Error(w, `{"error":"user_id is required"}`, http.StatusBadRequest)
			return
		}
		counts, err := sink.PurgeUser(r.Context(), req.UserID)
		if err != nil {
			http.Error(w, `{"error":`+mustJSON(err.Error())+`}`, http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(counts)
	})
	mux.HandleFunc("GET "+routes.DatalakeResidual, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		userID := r.URL.Query().Get("user_id")
		if userID == "" {
			http.Error(w, `{"error":"user_id is required"}`, http.StatusBadRequest)
			return
		}
		counts, err := sink.ResidualUser(r.Context(), userID)
		if err != nil {
			http.Error(w, `{"error":`+mustJSON(err.Error())+`}`, http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(counts)
	})
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// registerResetEndpoint mounts POST /v1/datalake/reset: drop every sink
// table (objects + delta log) and clear buffered rows — the lake leg of a
// full harness reset. Without it, e2e-era lake data accumulates forever and
// full-scan endpoints (residual) grow arbitrarily slow (35s+ after the
// 2026-07-18 hour run). Internal service surface, like purge/compact.
func registerResetEndpoint(mux *http.ServeMux, sink *datalakeSink) {
	mux.HandleFunc("POST "+routes.DatalakeReset, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		ctx := r.Context()
		out := map[string]string{}
		for _, table := range sink.Tables() {
			sink.dropBuffered(table)
			if err := sink.lake.TruncateTable(ctx, table); err != nil {
				out[table] = err.Error()
				continue
			}
			out[table] = "reset"
		}
		_ = json.NewEncoder(w).Encode(out)
	})
}

// registerCompactEndpoint mounts POST /v1/datalake/compact: bin-pack every
// sink table's small Parquet files into one consolidated file each. This
// REPLACED the standalone cmd/compact CronJob — a compaction commit from a
// second process races the sink's flush on Delta version allocation (both
// derive the next version from the log-file count), and only the lake's
// single writer can serialize them (the shared ObjectStore lock does).
// Flushes each table first so the freshly-buffered rows join the pack.
// Triggered by the batch-conductor chain (or ops, manually).
func registerCompactEndpoint(mux *http.ServeMux, sink *datalakeSink) {
	mux.HandleFunc("POST "+routes.DatalakeCompact, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		ctx := r.Context()
		out := map[string]datalake.CompactResult{}
		for _, table := range sink.Tables() {
			sink.flushTable(ctx, table)
			res, err := sink.lake.Compact(ctx, table)
			if err != nil {
				// A table that was never written has no log — report, keep going.
				out[table] = datalake.CompactResult{Table: table}
				continue
			}
			out[table] = res
		}
		_ = json.NewEncoder(w).Encode(out)
	})
}

// registerVacuumEndpoint mounts POST /v1/datalake/vacuum: physically delete
// tombstoned Parquet files older than the grace window (default 10m —
// covers in-flight readers). The GDPR tail of PurgeRows: the filtered
// rewrite tombstones the pre-purge files, this reclaims their bytes. Runs
// here for the same single-writer reason as compact; triggered by the
// batch-conductor chain right after compact (which itself tombstones the
// packed files).
func registerVacuumEndpoint(mux *http.ServeMux, sink *datalakeSink) {
	mux.HandleFunc("POST "+routes.DatalakeVacuum, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		grace := 10 * time.Minute
		if g := r.URL.Query().Get("grace"); g != "" {
			parsed, err := time.ParseDuration(g)
			if err != nil {
				http.Error(w, `{"error":"invalid grace duration"}`, http.StatusBadRequest)
				return
			}
			grace = parsed
		}
		ctx := r.Context()
		out := map[string]datalake.VacuumResult{}
		for _, table := range sink.Tables() {
			res, err := sink.lake.Vacuum(ctx, table, grace)
			if err != nil {
				http.Error(w, `{"error":`+mustJSON(table+": "+err.Error())+`}`, http.StatusInternalServerError)
				return
			}
			out[table] = res
		}
		_ = json.NewEncoder(w).Encode(out)
	})
}

// lakeProfileSummary is the lake half of the staff profile view: which
// onboarding signals mention the id, and how much behaviour it has.
type lakeProfileSummary struct {
	ProfileSignals  []datalake.Record `json:"profile_signals"`  // capped
	BehaviourCounts map[string]int    `json:"behaviour_counts"` // kind → rows
}

// registerProfileEndpoint mounts GET /v1/datalake/profile?user_id= — the
// gateway's staff profile API calls this for the lake-side summary (the
// gateway can't read Delta itself; the lake reader lives here).
func registerProfileEndpoint(mux *http.ServeMux, sink *datalakeSink) {
	const maxSignalRows = 100
	mux.HandleFunc("GET "+routes.DatalakeProfile, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		userID := r.URL.Query().Get("user_id")
		if userID == "" {
			http.Error(w, `{"error":"user_id is required"}`, http.StatusBadRequest)
			return
		}
		ctx := r.Context()
		out := lakeProfileSummary{ProfileSignals: []datalake.Record{}, BehaviourCounts: map[string]int{}}

		sink.flushTable(ctx, profileSignalsTable)
		sigs, err := sink.lake.Read(ctx, profileSignalsTable, datalake.Filter{Columns: map[string]interface{}{"id_value": userID}})
		if err != nil {
			http.Error(w, `{"error":`+mustJSON(err.Error())+`}`, http.StatusInternalServerError)
			return
		}
		for _, rec := range sigs {
			if len(out.ProfileSignals) >= maxSignalRows {
				break
			}
			out.ProfileSignals = append(out.ProfileSignals, rec)
		}

		sink.flushTable(ctx, behaviourSignalsTable)
		match := lakeUserTables(userID)[behaviourSignalsTable]
		rows, err := sink.lake.Read(ctx, behaviourSignalsTable, datalake.Filter{})
		if err != nil {
			http.Error(w, `{"error":`+mustJSON(err.Error())+`}`, http.StatusInternalServerError)
			return
		}
		for _, rec := range rows {
			if match(rec) {
				kind, _ := rec["kind"].(string)
				out.BehaviourCounts[kind]++
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	})
}
