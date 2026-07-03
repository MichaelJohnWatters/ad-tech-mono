package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/lib/pq"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
)

// errQCPublisherNotOwned is returned when a create references a publisher the
// caller's account doesn't own → the handler maps it to 403.
var errQCPublisherNotOwned = errors.New("publisher not owned by account")

// qualityControlView is one quality_controls row as the publisher console sees
// it.
type qualityControlView struct {
	ID          string   `json:"id"`
	PublisherID string   `json:"publisher_id"`
	Type        string   `json:"type"`
	Values      []string `json:"values"`
}

type qualityControlInput struct {
	PublisherID string   `json:"publisher_id"`
	Type        string   `json:"type"`
	Values      []string `json:"values"`
}

type qualityControlStore interface {
	ListQualityControls(ctx context.Context, accountID string) ([]qualityControlView, error)
	// UpsertQualityControl creates or replaces the row for (publisher_id, type)
	// and returns its id. Must return errQCPublisherNotOwned if publisher_id
	// isn't owned by accountID.
	UpsertQualityControl(ctx context.Context, accountID string, in qualityControlInput) (id string, err error)
	// DeleteQualityControl removes a row owned by accountID; sql.ErrNoRows if
	// none matches for that tenant.
	DeleteQualityControl(ctx context.Context, accountID, id string) error
}

// validQCTypes mirrors the quality_controls.type CHECK constraint.
var validQCTypes = map[string]bool{
	"advertiser_blocklist": true, "category_blocklist": true, "creative_blocklist": true,
	"advertiser_allowlist": true, "domain_blocklist": true,
}

// qualityControlsHandler manages a publisher's inventory quality controls
// (allow/block lists): GET lists (quality:read), POST upserts one per
// (publisher, type) (quality:update), DELETE removes (quality:update).
// Tenant-scoped — every query filters by the caller's account and a create must
// reference a publisher the caller owns.
func qualityControlsHandler(store qualityControlStore, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if devTenantGuard(w, r, claims, []qualityControlView{}) {
			return
		}

		switch r.Method {
		case http.MethodGet:
			if !can(claims, "quality:read") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			controls, err := store.ListQualityControls(r.Context(), claims.AccountID)
			if err != nil {
				log.Error("quality control list failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(controls)

		case http.MethodPost:
			if !can(claims, "quality:update") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			var in qualityControlInput
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
				return
			}
			if in.PublisherID == "" {
				http.Error(w, `{"error":"publisher_id required"}`, http.StatusBadRequest)
				return
			}
			if !validQCTypes[in.Type] {
				http.Error(w, `{"error":"invalid type"}`, http.StatusBadRequest)
				return
			}
			in.Values = cleanEvents(in.Values) // trim/dedup/drop-empty (shared helper)
			id, err := store.UpsertQualityControl(r.Context(), claims.AccountID, in)
			if errors.Is(err, errQCPublisherNotOwned) {
				http.Error(w, `{"error":"forbidden: publisher not in your account"}`, http.StatusForbidden)
				return
			}
			if err != nil {
				log.Error("quality control upsert failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "type": in.Type, "values": in.Values})

		case http.MethodDelete:
			if !can(claims, "quality:update") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			id := strings.TrimSpace(r.URL.Query().Get("id"))
			if id == "" {
				http.Error(w, `{"error":"id query param required"}`, http.StatusBadRequest)
				return
			}
			err := store.DeleteQualityControl(r.Context(), claims.AccountID, id)
			if err == sql.ErrNoRows {
				http.Error(w, `{"error":"quality control not found"}`, http.StatusNotFound)
				return
			}
			if err != nil {
				log.Error("quality control delete failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "deleted"})

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

type pgQualityControlStore struct{ db *sql.DB }

func (s pgQualityControlStore) ListQualityControls(ctx context.Context, accountID string) ([]qualityControlView, error) {
	if s.db == nil {
		return nil, sql.ErrConnDone
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id::text, publisher_id::text, type, values FROM quality_controls
		 WHERE account_id = $1::uuid ORDER BY type`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []qualityControlView{}
	for rows.Next() {
		var v qualityControlView
		if err := rows.Scan(&v.ID, &v.PublisherID, &v.Type, pq.Array(&v.Values)); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s pgQualityControlStore) UpsertQualityControl(ctx context.Context, accountID string, in qualityControlInput) (string, error) {
	if s.db == nil {
		return "", sql.ErrConnDone
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	// The publisher must belong to the caller's account.
	var pubAccount string
	err = tx.QueryRowContext(ctx, `SELECT account_id::text FROM publishers WHERE id = $1::uuid`, in.PublisherID).Scan(&pubAccount)
	if err == sql.ErrNoRows || (err == nil && pubAccount != accountID) {
		return "", errQCPublisherNotOwned
	}
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, accountID); err != nil {
		return "", err
	}
	// One row per (publisher, type): replace the existing row's values if present.
	var id string
	err = tx.QueryRowContext(ctx,
		`UPDATE quality_controls SET values = $3, updated_at = now()
		 WHERE publisher_id = $1::uuid AND type = $2 RETURNING id::text`,
		in.PublisherID, in.Type, pq.Array(in.Values)).Scan(&id)
	if err == sql.ErrNoRows {
		if err = tx.QueryRowContext(ctx,
			`INSERT INTO quality_controls (publisher_id, account_id, type, values)
			 VALUES ($1::uuid, $2::uuid, $3, $4) RETURNING id::text`,
			in.PublisherID, accountID, in.Type, pq.Array(in.Values)).Scan(&id); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	return id, tx.Commit()
}

func (s pgQualityControlStore) DeleteQualityControl(ctx context.Context, accountID, id string) error {
	if s.db == nil {
		return sql.ErrConnDone
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM quality_controls WHERE id = $1::uuid AND account_id = $2::uuid`, id, accountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
