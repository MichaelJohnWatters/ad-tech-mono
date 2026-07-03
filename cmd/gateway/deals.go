package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
)

// errDealPublisherNotOwned is returned when a create references a publisher the
// caller's account doesn't own → the handler maps it to 403.
var errDealPublisherNotOwned = errors.New("publisher not owned by account")

type dealInput struct {
	PublisherID   string  `json:"publisher_id"`
	Name          string  `json:"name"`
	DealType      string  `json:"deal_type"`
	Price         float64 `json:"price"`
	PriceCurrency string  `json:"price_currency"`
}

type dealView struct {
	ID          string  `json:"id"`
	PublisherID string  `json:"publisher_id"`
	Name        string  `json:"name"`
	DealType    string  `json:"deal_type"`
	Price       float64 `json:"price"`
	Status      string  `json:"status"`
}

type dealStore interface {
	ListDeals(ctx context.Context, accountID string) ([]dealView, error)
	// CreateDeal must return errDealPublisherNotOwned if publisher_id isn't
	// owned by accountID.
	CreateDeal(ctx context.Context, accountID string, in dealInput) (id string, err error)
}

var validDealTypes = map[string]bool{"open": true, "pmp": true, "pg": true, "preferred": true}

// dealsHandler lists (GET, deals:read) and creates (POST, deals:create) deals,
// scoped to the caller's account. On create it publishes the deals cache
// invalidate so the exchange picks the deal up sub-second.
func dealsHandler(store dealStore, bus events.EventBus, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if devTenantGuard(w, r, claims, []dealView{}) {
			return
		}

		switch r.Method {
		case http.MethodGet:
			if !can(claims, "deals:read") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			deals, err := store.ListDeals(r.Context(), claims.AccountID)
			if err != nil {
				log.Error("deals list failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(deals)

		case http.MethodPost:
			if !can(claims, "deals:create") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			var in dealInput
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
				return
			}
			if in.PublisherID == "" || in.Name == "" {
				http.Error(w, `{"error":"publisher_id and name required"}`, http.StatusBadRequest)
				return
			}
			if in.DealType == "" {
				in.DealType = "pmp"
			}
			if !validDealTypes[in.DealType] {
				http.Error(w, `{"error":"deal_type must be open, pmp, pg or preferred"}`, http.StatusBadRequest)
				return
			}
			if in.Price < 0 {
				http.Error(w, `{"error":"price must be >= 0"}`, http.StatusBadRequest)
				return
			}
			id, err := store.CreateDeal(r.Context(), claims.AccountID, in)
			if errors.Is(err, errDealPublisherNotOwned) {
				http.Error(w, `{"error":"forbidden: publisher not in your account"}`, http.StatusForbidden)
				return
			}
			if err != nil {
				log.Error("deal create failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			if bus != nil {
				_ = bus.Publish(r.Context(), events.SubjectCacheInvalidateDeals, []byte(`{"source":"gateway","id":"`+id+`"}`))
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "active"})

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

type pgDealStore struct{ db *sql.DB }

func (s pgDealStore) ListDeals(ctx context.Context, accountID string) ([]dealView, error) {
	if s.db == nil {
		return nil, sql.ErrConnDone
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id::text, publisher_id::text, name, deal_type, COALESCE(price,0), status
		 FROM deals WHERE account_id = $1::uuid ORDER BY created_at DESC`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []dealView{}
	for rows.Next() {
		var d dealView
		if err := rows.Scan(&d.ID, &d.PublisherID, &d.Name, &d.DealType, &d.Price, &d.Status); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s pgDealStore) CreateDeal(ctx context.Context, accountID string, in dealInput) (string, error) {
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
		return "", errDealPublisherNotOwned
	}
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, accountID); err != nil {
		return "", fmt.Errorf("set tenant: %w", err)
	}
	cur := in.PriceCurrency
	if cur == "" {
		cur = "USD"
	}
	var id string
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO deals (publisher_id, account_id, name, deal_type, price, price_currency, status, created_at, updated_at)
		 VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, 'active', now(), now()) RETURNING id::text`,
		in.PublisherID, accountID, in.Name, in.DealType, in.Price, cur).Scan(&id); err != nil {
		return "", err
	}
	return id, tx.Commit()
}
