package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

// conversionConfig is an advertiser-defined conversion event. It's the setup
// layer in front of the tracker's /v1/t/conv endpoint: name a conversion, give
// it a default value + currency, and the handler hands back an embeddable pixel
// keyed to its event_type.
type conversionConfig struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	EventType    string  `json:"event_type"`    // purchase | signup | lead | custom
	DefaultValue float64 `json:"default_value"` // revenue baked into the pixel's rev param
	Currency     string  `json:"currency"`
	Status       string  `json:"status"`
	CreatedAt    string  `json:"created_at"`
	// Pixel/Snippet are derived (not stored): the ready-to-embed HTML pixel and
	// a JS variant, built from the tracker base URL + this config.
	Pixel   string `json:"pixel"`
	Snippet string `json:"snippet"`
}

// conversionInput is a create request. account_id is never read from the body —
// the handler binds to the authenticated tenant.
type conversionInput struct {
	Name         string  `json:"name"`
	EventType    string  `json:"event_type"`
	DefaultValue float64 `json:"default_value"`
	Currency     string  `json:"currency"`
}

// conversionStore is the tenant-scoped persistence the handler needs. Every
// method filters by accountID; List/Create/Delete only ever touch the caller's
// rows.
type conversionStore interface {
	List(ctx context.Context, accountID string) ([]conversionConfig, error)
	Create(ctx context.Context, accountID string, in conversionInput) (conversionConfig, error)
	Delete(ctx context.Context, accountID, id string) error
}

// validConversionEventType mirrors the migration's CHECK constraint so the
// handler rejects bad input with a 400 rather than a 500 from Postgres.
func validConversionEventType(t string) bool {
	switch t {
	case "purchase", "signup", "lead", "custom":
		return true
	}
	return false
}

// conversionsHandler serves the advertiser conversion-setup API:
//
//	GET    /v1/api/conversions      — list the account's conversion configs (each with an embed pixel)
//	POST   /v1/api/conversions      — define a conversion (name, event_type, default_value, currency)
//	DELETE /v1/api/conversions?id=  — remove one
//
// Tenant sessions only: every query is scoped to claims.AccountID, so a caller
// can never read or delete another tenant's conversions. trackerURL is the
// browser-reachable tracker base (keys.Gateway.PublicTrackerURL) — the pixel the
// advertiser embeds fires straight at /v1/t/conv, so we never hardcode localhost.
func conversionsHandler(store conversionStore, trackerURL string, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if devTenantGuard(w, r, claims, []conversionConfig{}) {
			return
		}
		if store == nil {
			http.Error(w, `{"error":"conversion store unavailable"}`, http.StatusServiceUnavailable)
			return
		}

		switch r.Method {
		case http.MethodGet:
			if !can(claims, "campaigns:read") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			list, err := store.List(r.Context(), claims.AccountID)
			if err != nil {
				log.Error("conversion list failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			for i := range list {
				list[i].Pixel = conversionPixel(trackerURL, list[i])
				list[i].Snippet = conversionSnippet(trackerURL, list[i])
			}
			_ = json.NewEncoder(w).Encode(list)

		case http.MethodPost:
			if !can(claims, "campaigns:create") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			var in conversionInput
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
				return
			}
			in.Name = strings.TrimSpace(in.Name)
			in.EventType = strings.TrimSpace(in.EventType)
			in.Currency = strings.TrimSpace(in.Currency)
			if in.Name == "" {
				http.Error(w, `{"error":"name is required"}`, http.StatusBadRequest)
				return
			}
			if in.EventType == "" {
				in.EventType = "custom"
			}
			if !validConversionEventType(in.EventType) {
				http.Error(w, `{"error":"event_type must be purchase, signup, lead or custom"}`, http.StatusBadRequest)
				return
			}
			if in.Currency == "" {
				in.Currency = "USD"
			}
			cfg, err := store.Create(r.Context(), claims.AccountID, in)
			if err != nil {
				log.Error("conversion create failed", "name", in.Name, "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			cfg.Pixel = conversionPixel(trackerURL, cfg)
			cfg.Snippet = conversionSnippet(trackerURL, cfg)
			log.Info("conversion created", "id", cfg.ID, "name", cfg.Name, "event_type", cfg.EventType)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(cfg)

		case http.MethodDelete:
			if !can(claims, "campaigns:delete") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			id := strings.TrimSpace(r.URL.Query().Get("id"))
			if id == "" {
				http.Error(w, `{"error":"id query param required"}`, http.StatusBadRequest)
				return
			}
			err := store.Delete(r.Context(), claims.AccountID, id)
			if err == sql.ErrNoRows {
				http.Error(w, `{"error":"conversion not found"}`, http.StatusNotFound)
				return
			}
			if err != nil {
				log.Error("conversion delete failed", "id", id, "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "deleted"})

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

// conversionURL builds the /v1/t/conv URL for a config. The advertiser embeds
// this on THEIR OWN site (order-confirmation / thank-you page), substituting the
// trace id they captured for the __TRACE_ID__ placeholder. It fires straight at
// the tracker — the same params the conv handler reads (tid=trace, type, rev,
// cur). Note: unlike serve-chain beacons, this pixel carries no HMAC sig — a
// third-party page can't sign, so the tracker's signature_validation must stay
// off (its default) for advertiser conversions, exactly like the retargeting
// pixel.
func conversionURL(trackerURL string, c conversionConfig) string {
	q := url.Values{}
	q.Set("tid", "__TRACE_ID__") // placeholder — the advertiser substitutes the captured trace/click id
	q.Set("type", c.EventType)
	q.Set("rev", strconv.FormatFloat(c.DefaultValue, 'f', -1, 64))
	q.Set("cur", c.Currency)
	return strings.TrimRight(trackerURL, "/") + routes.TrackerConversion + "?" + q.Encode()
}

// conversionPixel is the ready-to-embed 1x1 image tag.
func conversionPixel(trackerURL string, c conversionConfig) string {
	return fmt.Sprintf(`<img src="%s" width="1" height="1" style="display:none" alt=""/>`,
		conversionURL(trackerURL, c))
}

// conversionSnippet is the JS variant — fires the same beacon from script (e.g.
// after a client-side purchase completes).
func conversionSnippet(trackerURL string, c conversionConfig) string {
	return fmt.Sprintf(`<script>(function(){var i=new Image(1,1);i.src=%q;})();</script>`,
		conversionURL(trackerURL, c))
}

// pgConversionStore is the Postgres-backed conversionStore. Writes run inside a
// tenant transaction (app.current_account_id set) so RLS admits them and can't
// touch another tenant's rows; reads add an explicit account_id filter on top of
// RLS. Mirrors the audience store's withTenant pattern.
type pgConversionStore struct{ db *sql.DB }

func (s pgConversionStore) List(ctx context.Context, accountID string) ([]conversionConfig, error) {
	out := []conversionConfig{}
	if s.db == nil {
		return out, sql.ErrConnDone
	}
	const q = `
SELECT id::text, name, event_type, default_value, currency, status, created_at
FROM conversion_configs
WHERE account_id = $1::uuid
ORDER BY created_at DESC`
	// Tenant GUC must be set or RLS silently blanks the rows under the
	// NOBYPASSRLS app role (security #77).
	rows, closeFn, err := postgres.QueryTenantDB(ctx, s.db, accountID, q, accountID)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	for rows.Next() {
		var c conversionConfig
		var created sql.NullTime
		if err := rows.Scan(&c.ID, &c.Name, &c.EventType, &c.DefaultValue,
			&c.Currency, &c.Status, &created); err != nil {
			return nil, err
		}
		if created.Valid {
			c.CreatedAt = created.Time.Format("2006-01-02T15:04:05Z07:00")
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s pgConversionStore) Create(ctx context.Context, accountID string, in conversionInput) (conversionConfig, error) {
	var c conversionConfig
	if s.db == nil {
		return c, sql.ErrConnDone
	}
	err := s.withTenant(ctx, accountID, func(tx *sql.Tx) error {
		const q = `
INSERT INTO conversion_configs (account_id, name, event_type, default_value, currency)
VALUES ($1::uuid, $2, $3, $4, $5)
RETURNING id::text, name, event_type, default_value, currency, status, created_at`
		var created sql.NullTime
		if err := tx.QueryRowContext(ctx, q, accountID, in.Name, in.EventType, in.DefaultValue, in.Currency).
			Scan(&c.ID, &c.Name, &c.EventType, &c.DefaultValue, &c.Currency, &c.Status, &created); err != nil {
			return err
		}
		if created.Valid {
			c.CreatedAt = created.Time.Format("2006-01-02T15:04:05Z07:00")
		}
		return nil
	})
	if err != nil {
		return conversionConfig{}, err
	}
	return c, nil
}

func (s pgConversionStore) Delete(ctx context.Context, accountID, id string) error {
	if s.db == nil {
		return sql.ErrConnDone
	}
	return s.withTenant(ctx, accountID, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM conversion_configs WHERE id = $1::uuid AND account_id = $2::uuid`, id, accountID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return sql.ErrNoRows
		}
		return nil
	})
}

// withTenant runs fn in a transaction with the RLS tenant GUC set, so writes
// into conversion_configs are admitted (and scoped to accountID).
func (s pgConversionStore) withTenant(ctx context.Context, accountID string, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, accountID); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
