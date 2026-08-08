package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

// creativeInput is an advertiser creative upload.
type creativeInput struct {
	Name        string `json:"name"`
	Format      string `json:"format"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	HTMLContent string `json:"html_content"`
	LandingURL  string `json:"landing_url"`
}

// creativeView is one row of the advertiser's creative library — includes
// review state so the portal can show pending/rejected alongside approved
// (the adserver proxy at the APICreatives subtree only serves approved).
type creativeView struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Format          string `json:"format"`
	Width           int    `json:"width"`
	Height          int    `json:"height"`
	LandingURL      string `json:"landing_url"`
	ReviewStatus    string `json:"review_status"`
	RejectionReason string `json:"rejection_reason"`
	CreatedAt       string `json:"created_at"`
}

// creativeStore persists uploads and lists the tenant's library.
type creativeStore interface {
	CreateCreative(ctx context.Context, accountID string, in creativeInput) (id string, err error)
	ListCreatives(ctx context.Context, accountID string) ([]creativeView, error)
}

var validCreativeFormats = map[string]bool{"display": true, "native": true, "video": true, "audio": true}

// creativeUploadHandler is the advertiser creative library: GET lists the
// caller's creatives with review state (creatives:read), POST uploads one
// with review_status='pending_review' → the staff moderation queue
// (creatives:upload). Tenant-scoped from claims.
func creativeUploadHandler(store creativeStore, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if devTenantGuard(w, r, claims, []creativeView{}) {
			return
		}

		switch r.Method {
		case http.MethodGet:
			if !can(claims, "creatives:read") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			list, err := store.ListCreatives(r.Context(), claims.AccountID)
			if err != nil {
				log.Error("creative list failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(list)
			return
		case http.MethodPost:
			// fallthrough to the upload flow below
		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}

		if !can(claims, "creatives:upload") {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}

		var in creativeInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
			return
		}
		if in.Name == "" || in.LandingURL == "" {
			http.Error(w, `{"error":"name and landing_url are required"}`, http.StatusBadRequest)
			return
		}
		if in.Format == "" {
			in.Format = "display"
		}
		if !validCreativeFormats[in.Format] {
			http.Error(w, `{"error":"format must be display, native, video or audio"}`, http.StatusBadRequest)
			return
		}
		id, err := store.CreateCreative(r.Context(), claims.AccountID, in)
		if err != nil {
			log.Error("creative upload failed", "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "review_status": "pending_review"})
	}
}

// pgCreativeStore is the Postgres-backed creativeStore.
type pgCreativeStore struct{ db *sql.DB }

func (s pgCreativeStore) CreateCreative(ctx context.Context, accountID string, in creativeInput) (string, error) {
	if s.db == nil {
		return "", sql.ErrConnDone
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, accountID); err != nil {
		return "", fmt.Errorf("set tenant: %w", err)
	}
	var width, height any
	if in.Width > 0 {
		width = in.Width
	}
	if in.Height > 0 {
		height = in.Height
	}
	var htmlContent any
	if in.HTMLContent != "" {
		htmlContent = in.HTMLContent
	}
	var id string
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO creatives (account_id, name, format, width, height, html_content, landing_url, review_status, created_at, updated_at)
		 VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, 'pending_review', now(), now()) RETURNING id::text`,
		accountID, in.Name, in.Format, width, height, htmlContent, in.LandingURL).Scan(&id); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

func (s pgCreativeStore) ListCreatives(ctx context.Context, accountID string) ([]creativeView, error) {
	if s.db == nil {
		return nil, sql.ErrConnDone
	}
	// Tenant GUC must be set or RLS silently blanks the rows under the
	// NOBYPASSRLS app role (security #77).
	rows, closeFn, err := postgres.QueryTenantDB(ctx, s.db, accountID,
		`SELECT id::text, name, format, COALESCE(width, 0), COALESCE(height, 0),
		        COALESCE(landing_url, ''), review_status, COALESCE(rejection_reason, ''),
		        created_at::text
		 FROM creatives WHERE account_id = $1::uuid
		 ORDER BY created_at DESC LIMIT 500`, accountID)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	out := []creativeView{}
	for rows.Next() {
		var c creativeView
		if err := rows.Scan(&c.ID, &c.Name, &c.Format, &c.Width, &c.Height,
			&c.LandingURL, &c.ReviewStatus, &c.RejectionReason, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
