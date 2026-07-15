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
