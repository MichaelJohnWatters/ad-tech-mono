package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
)

// auditEntryView is one audit_log row as the staff console renders it.
type auditEntryView struct {
	ID           string          `json:"id"`
	Timestamp    time.Time       `json:"timestamp"`
	AccountID    string          `json:"account_id"`
	ActorID      string          `json:"actor_id"`
	Action       string          `json:"action"`
	ResourceType string          `json:"resource_type"`
	ResourceID   string          `json:"resource_id"`
	Reason       string          `json:"reason"`
	Changes      json.RawMessage `json:"changes"`
}

// auditQuery is the filter set the viewer supports. Empty fields are
// unfiltered. All matching is exact — the audit log is queried by known
// values (an action name, a resource id from another screen), not searched.
type auditQuery struct {
	AccountID    string
	Action       string
	ResourceType string
	ResourceID   string
	Limit        int
}

type auditLogStore interface {
	ListAudit(ctx context.Context, q auditQuery) ([]auditEntryView, error)
}

// auditLogHandler serves the staff audit-log viewer (GET, audit:read).
// Platform-wide by design: the audit trail is an operator tool, so it's
// permission-gated rather than tenant-scoped — customer roles don't carry
// audit:read.
func auditLogHandler(store auditLogStore, log *slog.Logger) http.HandlerFunc {
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
		if !can(claims, "audit:read") {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")

		q := auditQuery{
			AccountID:    strings.TrimSpace(r.URL.Query().Get("account_id")),
			Action:       strings.TrimSpace(r.URL.Query().Get("action")),
			ResourceType: strings.TrimSpace(r.URL.Query().Get("resource_type")),
			ResourceID:   strings.TrimSpace(r.URL.Query().Get("resource_id")),
			Limit:        100,
		}
		if v := r.URL.Query().Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				http.Error(w, `{"error":"limit must be a positive integer"}`, http.StatusBadRequest)
				return
			}
			q.Limit = n
		}
		if q.Limit > 500 {
			q.Limit = 500
		}
		if q.AccountID != "" && !uuidRe.MatchString(q.AccountID) {
			http.Error(w, `{"error":"account_id must be a UUID"}`, http.StatusBadRequest)
			return
		}

		entries, err := store.ListAudit(r.Context(), q)
		if err != nil {
			log.Error("audit list failed", "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(entries)
	}
}

type pgAuditLogStore struct{ db *sql.DB }

func (s pgAuditLogStore) ListAudit(ctx context.Context, q auditQuery) ([]auditEntryView, error) {
	if s.db == nil {
		return nil, sql.ErrConnDone
	}
	// Filters compose as indexed positional predicates; empty filter values
	// short-circuit via the OR so one prepared shape serves every combination.
	rows, err := s.db.QueryContext(ctx, `
SELECT id::text, timestamp, COALESCE(account_id::text, ''), actor_id, action,
       resource_type, resource_id, COALESCE(reason, ''), COALESCE(changes, '[]'::jsonb)
FROM audit_log
WHERE ($1 = '' OR account_id = $1::uuid)
  AND ($2 = '' OR action = $2)
  AND ($3 = '' OR resource_type = $3)
  AND ($4 = '' OR resource_id = $4)
ORDER BY timestamp DESC
LIMIT $5`,
		q.AccountID, q.Action, q.ResourceType, q.ResourceID, q.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []auditEntryView{}
	for rows.Next() {
		var e auditEntryView
		var changes []byte
		if err := rows.Scan(&e.ID, &e.Timestamp, &e.AccountID, &e.ActorID, &e.Action,
			&e.ResourceType, &e.ResourceID, &e.Reason, &changes); err != nil {
			return nil, err
		}
		e.Changes = json.RawMessage(changes)
		out = append(out, e)
	}
	return out, rows.Err()
}
