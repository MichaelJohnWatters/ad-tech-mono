package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
)

// moderationItem is a creative awaiting review, as the staff queue sees it.
type moderationItem struct {
	ID           string    `json:"id"`
	AccountID    string    `json:"account_id"`
	Advertiser   string    `json:"advertiser"`
	Name         string    `json:"name"`
	Format       string    `json:"format"`
	ReviewStatus string    `json:"review_status"`
	CreatedAt    time.Time `json:"created_at"`
}

type moderationStore interface {
	ListPending(ctx context.Context) ([]moderationItem, error)
	// Decide sets a creative's review_status (approved/rejected) with an
	// optional reason and reviewer. Returns sql.ErrNoRows if the id is unknown.
	Decide(ctx context.Context, creativeID, newStatus, reason, reviewerID string) error
}

// moderationHandler is the staff review queue: GET lists pending creatives
// (moderation:read), POST decides one (moderation:approve / moderation:reject).
// Platform-wide — staff/admin review across all accounts, so it's permission-
// gated, not tenant-scoped.
func moderationHandler(store moderationStore, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")

		switch r.Method {
		case http.MethodGet:
			if !can(claims, "moderation:read") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			items, err := store.ListPending(r.Context())
			if err != nil {
				log.Error("moderation list failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(items)

		case http.MethodPost:
			var req struct {
				CreativeID string `json:"creative_id"`
				Action     string `json:"action"` // approve | reject
				Reason     string `json:"reason"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
				return
			}
			if req.CreativeID == "" {
				http.Error(w, `{"error":"creative_id required"}`, http.StatusBadRequest)
				return
			}
			var newStatus, perm string
			switch req.Action {
			case "approve":
				newStatus, perm = "approved", "moderation:approve"
			case "reject":
				newStatus, perm = "rejected", "moderation:reject"
				if strings.TrimSpace(req.Reason) == "" {
					http.Error(w, `{"error":"reason required to reject"}`, http.StatusBadRequest)
					return
				}
			default:
				http.Error(w, `{"error":"action must be approve or reject"}`, http.StatusBadRequest)
				return
			}
			if !can(claims, perm) {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			reviewer := strings.TrimPrefix(claims.UserID, "user-")
			err := store.Decide(r.Context(), req.CreativeID, newStatus, req.Reason, reviewer)
			if err == sql.ErrNoRows {
				http.Error(w, `{"error":"creative not found"}`, http.StatusNotFound)
				return
			}
			if err != nil {
				log.Error("moderation decide failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"id": req.CreativeID, "review_status": newStatus})

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

// asUUID returns s as a query arg only if it looks like a UUID, else nil (→
// NULL). Guards against casting the dev "dev-user" id into a uuid column.
func asUUID(s string) any {
	if len(s) == 36 && s[8] == '-' && s[13] == '-' && s[18] == '-' && s[23] == '-' {
		return s
	}
	return nil
}

type pgModerationStore struct{ db *sql.DB }

func (s pgModerationStore) ListPending(ctx context.Context) ([]moderationItem, error) {
	if s.db == nil {
		return nil, sql.ErrConnDone
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.id::text, c.account_id::text, a.name, c.name, c.format, c.review_status, c.created_at
		 FROM creatives c JOIN accounts a ON a.id = c.account_id
		 WHERE c.review_status = 'pending_review' ORDER BY c.created_at LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []moderationItem{}
	for rows.Next() {
		var m moderationItem
		if err := rows.Scan(&m.ID, &m.AccountID, &m.Advertiser, &m.Name, &m.Format, &m.ReviewStatus, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s pgModerationStore) Decide(ctx context.Context, creativeID, newStatus, reason, reviewerID string) error {
	if s.db == nil {
		return sql.ErrConnDone
	}
	var reasonArg any
	if reason != "" {
		reasonArg = reason
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE creatives SET review_status = $2, rejection_reason = $3, reviewed_by = $4::uuid, reviewed_at = now(), updated_at = now()
		 WHERE id = $1::uuid`,
		creativeID, newStatus, reasonArg, asUUID(reviewerID))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
