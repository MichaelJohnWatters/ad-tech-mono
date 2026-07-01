package main

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
)

// optOutRequest is a user privacy opt-out: level 1 (no personalisation),
// 2 (no tracking), or 3 (full deletion). Source records where it came from
// (e.g. "dsar_portal", "gpc", "support").
type optOutRequest struct {
	UserID string `json:"user_id"`
	Level  int    `json:"level"`
	Source string `json:"source,omitempty"`
}

// privacyOptOutHandler is the intake for POST /v1/api/privacy/optout. It
// records the opt-out in opt_out_registry, publishes the OptOutEvent, and
// invalidates the opt-out warm caches so enforcers (DSP no-bid / segment strip,
// tracker pixel reject) pick it up within a round-trip instead of a poll. This
// is the missing write half — enforcement already existed, ingestion didn't.
func privacyOptOutHandler(db *sql.DB, bus events.EventBus, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if db == nil {
			http.Error(w, "opt-out store unavailable", http.StatusServiceUnavailable)
			return
		}
		var req optOutRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if req.UserID == "" {
			http.Error(w, "user_id is required", http.StatusBadRequest)
			return
		}
		if req.Level < 1 || req.Level > 3 {
			http.Error(w, "level must be 1, 2, or 3", http.StatusBadRequest)
			return
		}
		if req.Source == "" {
			req.Source = "api"
		}

		const q = `
INSERT INTO opt_out_registry (user_id, level, source, requested_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (user_id) DO UPDATE SET level = EXCLUDED.level, source = EXCLUDED.source, requested_at = now()`
		if _, err := db.ExecContext(r.Context(), q, req.UserID, req.Level, req.Source); err != nil {
			log.Error("privacy opt-out: registry write failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// Publish the record event (for future consumers / audit) and the
		// cache-invalidate (immediate reload by the DSP opt-out warm cache).
		if bus != nil {
			pub := events.NewPublisher(bus, log)
			if err := pub.OptOut(r.Context(), events.OptOutEvent{
				UserID: req.UserID, Level: req.Level, Source: req.Source, Timestamp: time.Now(),
			}); err != nil {
				log.Warn("privacy opt-out: publish OptOutEvent failed", "error", err)
			}
			payload := []byte(`{"user_id":"` + req.UserID + `"}`)
			if err := bus.Publish(r.Context(), events.SubjectCacheInvalidateOptOuts, payload); err != nil {
				log.Warn("privacy opt-out: invalidate publish failed", "error", err)
			}
		}

		log.Info("privacy opt-out recorded", "user_id", req.UserID, "level", req.Level, "source", req.Source)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"user_id": req.UserID, "level": req.Level, "status": "recorded"})
	}
}
