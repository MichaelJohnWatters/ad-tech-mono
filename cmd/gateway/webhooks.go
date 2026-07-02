package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/lib/pq"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
)

// webhookView is one webhooks row as the account console sees it. The HMAC
// secret is returned only once, on create — never listed.
type webhookView struct {
	ID     string   `json:"id"`
	URL    string   `json:"url"`
	Events []string `json:"events"`
	Status string   `json:"status"`
}

type webhookInput struct {
	URL    string   `json:"url"`
	Events []string `json:"events"`
}

type webhookStore interface {
	ListWebhooks(ctx context.Context, accountID string) ([]webhookView, error)
	// CreateWebhook inserts a subscription with a freshly-generated HMAC secret
	// and returns (id, secret). The secret is shown to the caller once.
	CreateWebhook(ctx context.Context, accountID string, in webhookInput, secret string) (id string, err error)
	// DeleteWebhook removes a subscription owned by accountID; returns
	// sql.ErrNoRows if no such row exists for that tenant.
	DeleteWebhook(ctx context.Context, accountID, id string) error
}

// webhooksHandler manages a caller's webhook subscriptions: GET lists
// (webhooks:read), POST creates (webhooks:create), DELETE removes
// (webhooks:delete). Tenant-scoped — every query filters by the caller's
// account. Mutations publish the webhook-subs cache invalidate so the webhooks
// dispatcher reloads its warm cache sub-second.
func webhooksHandler(store webhookStore, bus events.EventBus, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")

		switch r.Method {
		case http.MethodGet:
			if !can(claims, "webhooks:read") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			hooks, err := store.ListWebhooks(r.Context(), claims.AccountID)
			if err != nil {
				log.Error("webhook list failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(hooks)

		case http.MethodPost:
			if !can(claims, "webhooks:create") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			var in webhookInput
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
				return
			}
			in.URL = strings.TrimSpace(in.URL)
			if !strings.HasPrefix(in.URL, "http://") && !strings.HasPrefix(in.URL, "https://") {
				http.Error(w, `{"error":"url must be an http(s) URL"}`, http.StatusBadRequest)
				return
			}
			in.Events = cleanEvents(in.Events)
			if len(in.Events) == 0 {
				http.Error(w, `{"error":"at least one event required"}`, http.StatusBadRequest)
				return
			}
			secret := randomToken()
			id, err := store.CreateWebhook(r.Context(), claims.AccountID, in, secret)
			if err != nil {
				log.Error("webhook create failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			publishWebhookInvalidate(r.Context(), bus, id)
			w.WriteHeader(http.StatusCreated)
			// secret is returned once, here, and never listed again.
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "url": in.URL, "events": in.Events, "secret": secret})

		case http.MethodDelete:
			if !can(claims, "webhooks:delete") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			id := strings.TrimSpace(r.URL.Query().Get("id"))
			if id == "" {
				http.Error(w, `{"error":"id query param required"}`, http.StatusBadRequest)
				return
			}
			err := store.DeleteWebhook(r.Context(), claims.AccountID, id)
			if err == sql.ErrNoRows {
				http.Error(w, `{"error":"webhook not found"}`, http.StatusNotFound)
				return
			}
			if err != nil {
				log.Error("webhook delete failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			publishWebhookInvalidate(r.Context(), bus, id)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "deleted"})

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

// cleanEvents trims, drops empties, and de-dups the requested event list.
func cleanEvents(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, e := range in {
		e = strings.TrimSpace(e)
		if e == "" || seen[e] {
			continue
		}
		seen[e] = true
		out = append(out, e)
	}
	return out
}

func publishWebhookInvalidate(ctx context.Context, bus events.EventBus, id string) {
	if bus == nil {
		return
	}
	_ = bus.Publish(ctx, events.SubjectCacheInvalidateWebhookSubs, []byte(`{"source":"gateway","id":"`+id+`"}`))
}

type pgWebhookStore struct{ db *sql.DB }

func (s pgWebhookStore) ListWebhooks(ctx context.Context, accountID string) ([]webhookView, error) {
	if s.db == nil {
		return nil, sql.ErrConnDone
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id::text, url, events, status FROM webhooks WHERE account_id = $1::uuid ORDER BY created_at DESC`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []webhookView{}
	for rows.Next() {
		var v webhookView
		if err := rows.Scan(&v.ID, &v.URL, pq.Array(&v.Events), &v.Status); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s pgWebhookStore) CreateWebhook(ctx context.Context, accountID string, in webhookInput, secret string) (string, error) {
	if s.db == nil {
		return "", sql.ErrConnDone
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, accountID); err != nil {
		return "", err
	}
	var id string
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO webhooks (account_id, url, events, secret, status)
		 VALUES ($1::uuid, $2, $3, $4, 'active') RETURNING id::text`,
		accountID, in.URL, pq.Array(in.Events), secret).Scan(&id); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

func (s pgWebhookStore) DeleteWebhook(ctx context.Context, accountID, id string) error {
	if s.db == nil {
		return sql.ErrConnDone
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM webhooks WHERE id = $1::uuid AND account_id = $2::uuid`, id, accountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
