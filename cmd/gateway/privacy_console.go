package main

// Staff privacy console — the READ surface over the opt-out registry the
// operator-key intake (privacy.go) writes to, plus a staff intake wrapper for
// the support workflow ("a user emailed asking to opt out"). All JWT-gated on
// support:* — the operator-key POST at routes.APIPrivacyOptOut is untouched.
//
// The registry is platform-global (opt_out_registry has no account_id), but
// reads still go through the postgres platform hatch so they keep working
// unchanged if the table ever grows an RLS policy under the NOBYPASSRLS app
// role (security #77) — the same posture as every other staff cross-tenant read
// (see moderation.go ListPending).

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/audit"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

// privacyOptOutView is one opt_out_registry row as the staff console renders
// it. Level semantics (docs/PLAN.md → "User Opt-Out and Data Deletion System"):
// 1 = no personalisation (contextual only), 2 = no tracking (cookie deleted,
// no freq caps / identity graph / user-level events), 3 = full deletion
// (GDPR/CCPA erasure, irreversible).
type privacyOptOutView struct {
	UserID      string     `json:"user_id"`
	Level       int        `json:"level"`
	Source      string     `json:"source"`
	RequestedAt time.Time  `json:"requested_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	VerifiedAt  *time.Time `json:"verified_at,omitempty"`
}

type privacyConsoleStore interface {
	// Lookup returns the registry row for an identity, sql.ErrNoRows when the
	// identity never opted out.
	Lookup(ctx context.Context, id string) (privacyOptOutView, error)
	// Recent returns the latest opt-outs, newest first.
	Recent(ctx context.Context, limit int) ([]privacyOptOutView, error)
}

// privacyStatusHandler serves GET /v1/api/privacy/status?id=… — the staff
// per-identity opt-out lookup. A missing row is a valid answer (opted_out
// false, level 0), not a 404, so the console can render "no opt-out on file".
func privacyStatusHandler(store privacyConsoleStore, log *slog.Logger) http.HandlerFunc {
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
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" {
			http.Error(w, `{"error":"id is required"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		row, err := store.Lookup(r.Context(), id)
		if errors.Is(err, sql.ErrNoRows) {
			_ = json.NewEncoder(w).Encode(map[string]any{"user_id": id, "opted_out": false, "level": 0})
			return
		}
		if err != nil {
			log.Error("privacy console: status lookup failed", "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(struct {
			OptedOut bool `json:"opted_out"`
			privacyOptOutView
		}{true, row})
	}
}

// privacyOptOutsHandler serves /v1/api/privacy/optouts:
//
//	GET  (support:read)   — the recent-opt-outs list for the staff console.
//	POST (support:update) — staff intake on behalf of a user; delegates to the
//	                        SAME recorder as the operator-key endpoint (write +
//	                        OptOutEvent + cache invalidate), so enforcement
//	                        semantics can't drift between the two paths.
func privacyOptOutsHandler(store privacyConsoleStore, intake http.HandlerFunc, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodGet:
			if !can(claims, "support:read") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			list, err := store.Recent(r.Context(), 100)
			if err != nil {
				log.Error("privacy console: recent list failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(list)

		case http.MethodPost:
			if !can(claims, "support:update") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			if intake == nil {
				http.Error(w, `{"error":"opt-out intake unavailable"}`, http.StatusServiceUnavailable)
				return
			}
			// A staff-recorded opt-out is a GDPR-relevant action (level 3 is
			// irreversible erasure) — it must NOT land indistinguishable from an
			// operator-API intake. Force source to the acting staff identity
			// (whatever the form sent) and leave an audit trail before
			// delegating to the shared recorder.
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
				return
			}
			body["source"] = "staff:" + claims.UserID
			raw, err := json.Marshal(body)
			if err != nil {
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(raw))
			r.ContentLength = int64(len(raw))
			userID, _ := body["user_id"].(string)
			_ = audit.Log(r.Context(), privacyAuditDB(store), audit.Entry{
				ActorID: claims.UserID, Action: "privacy:optout_recorded",
				ResourceType: "opt_out", ResourceID: userID,
				Changes: map[string]any{"level": body["level"], "source": body["source"]},
			})
			intake(w, r)

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

// privacyAuditDB pulls the *sql.DB from the pg store so the staff intake can
// write the audit row (same pattern as auditDBFrom in payout_method.go); nil
// for the test fake, which audit.Log tolerates.
func privacyAuditDB(store privacyConsoleStore) *sql.DB {
	if pg, ok := store.(pgPrivacyConsoleStore); ok {
		return pg.db
	}
	return nil
}

type pgPrivacyConsoleStore struct{ db *sql.DB }

func (s pgPrivacyConsoleStore) Lookup(ctx context.Context, id string) (privacyOptOutView, error) {
	var v privacyOptOutView
	if s.db == nil {
		return v, sql.ErrConnDone
	}
	err := postgres.NewFromDB(s.db).QueryRowPlatform(ctx, func(row *sql.Row) error {
		return row.Scan(&v.UserID, &v.Level, &v.Source, &v.RequestedAt, &v.CompletedAt, &v.VerifiedAt)
	}, `SELECT user_id, level, source, requested_at, completed_at, verified_at
	    FROM opt_out_registry WHERE user_id = $1`, id)
	return v, err
}

func (s pgPrivacyConsoleStore) Recent(ctx context.Context, limit int) ([]privacyOptOutView, error) {
	if s.db == nil {
		return nil, sql.ErrConnDone
	}
	rows, closeFn, err := postgres.NewFromDB(s.db).QueryPlatform(ctx,
		`SELECT user_id, level, source, requested_at, completed_at, verified_at
		 FROM opt_out_registry ORDER BY requested_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	out := []privacyOptOutView{}
	for rows.Next() {
		var v privacyOptOutView
		if err := rows.Scan(&v.UserID, &v.Level, &v.Source, &v.RequestedAt, &v.CompletedAt, &v.VerifiedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
