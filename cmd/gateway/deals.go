package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
	"github.com/lib/pq"
)

func derefStrings(p *[]string) []string {
	if p == nil {
		return nil
	}
	return *p
}
func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// errDealPublisherNotOwned is returned when a create references a publisher the
// caller's account doesn't own → the handler maps it to 403.
var errDealPublisherNotOwned = errors.New("publisher not owned by account")

type dealInput struct {
	PublisherID   string  `json:"publisher_id"`
	Name          string  `json:"name"`
	DealType      string  `json:"deal_type"`
	Price         float64 `json:"price"`
	PriceCurrency string  `json:"price_currency"`
	// AdvertiserIDs / PlacementIDs are the deal's allowlists — empty means
	// "any" (the exchange matcher treats an empty list as match-all; see
	// pkg/deals). AdvertiserIDs are the external advertiser accounts the
	// publisher grants access to; PlacementIDs must be the publisher's own.
	AdvertiserIDs []string `json:"advertiser_ids,omitempty"`
	PlacementIDs  []string `json:"placement_ids,omitempty"`
	// GuaranteedVolume is the committed impression count for PG deals.
	GuaranteedVolume int64 `json:"guaranteed_volume,omitempty"`
	// Flight window (YYYY-MM-DD). Empty = always active.
	StartDate string `json:"start_date,omitempty"`
	EndDate   string `json:"end_date,omitempty"`
}

type dealView struct {
	ID               string   `json:"id"`
	PublisherID      string   `json:"publisher_id"`
	Name             string   `json:"name"`
	DealType         string   `json:"deal_type"`
	Price            float64  `json:"price"`
	Status           string   `json:"status"`
	AdvertiserIDs    []string `json:"advertiser_ids"`
	PlacementIDs     []string `json:"placement_ids"`
	GuaranteedVolume int64    `json:"guaranteed_volume"`
	StartDate        string   `json:"start_date,omitempty"`
	EndDate          string   `json:"end_date,omitempty"`
}

// dealPatchInput is a partial update — nil fields are left unchanged.
type dealPatchInput struct {
	Name             *string   `json:"name,omitempty"`
	Price            *float64  `json:"price,omitempty"`
	Status           *string   `json:"status,omitempty"` // active | paused
	AdvertiserIDs    *[]string `json:"advertiser_ids,omitempty"`
	PlacementIDs     *[]string `json:"placement_ids,omitempty"`
	GuaranteedVolume *int64    `json:"guaranteed_volume,omitempty"`
	StartDate        *string   `json:"start_date,omitempty"`
	EndDate          *string   `json:"end_date,omitempty"`
}

// errDealPlacementNotOwned → 403: a placement allowlist referenced a
// placement the caller's account doesn't own.
var errDealPlacementNotOwned = errors.New("placement not owned by account")

// validateDealAllowlists checks UUID format for both allowlists and the
// date window. Placement ownership is enforced in the store (needs the DB).
func validateDealAllowlists(advIDs, plIDs []string, start, end string) error {
	for _, id := range advIDs {
		if !uuidRe.MatchString(id) {
			return fmt.Errorf("advertiser_ids must be UUIDs, got %q", id)
		}
	}
	for _, id := range plIDs {
		if !uuidRe.MatchString(id) {
			return fmt.Errorf("placement_ids must be UUIDs, got %q", id)
		}
	}
	if !dealValidDate(start) || !dealValidDate(end) {
		return errors.New("start_date and end_date must be YYYY-MM-DD when set")
	}
	if start != "" && end != "" && end < start {
		return errors.New("end_date must be on or after start_date")
	}
	return nil
}

func dealValidDate(s string) bool {
	if s == "" {
		return true
	}
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

type dealStore interface {
	ListDeals(ctx context.Context, accountID string) ([]dealView, error)
	// CreateDeal must return errDealPublisherNotOwned if publisher_id isn't
	// owned by accountID.
	CreateDeal(ctx context.Context, accountID string, in dealInput) (id string, err error)
	// UpdateDeal patches a deal owned by accountID; sql.ErrNoRows if the id
	// is unknown or belongs to another tenant (indistinguishable on purpose).
	UpdateDeal(ctx context.Context, accountID, id string, in dealPatchInput) error
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
			if in.GuaranteedVolume < 0 {
				http.Error(w, `{"error":"guaranteed_volume must be >= 0"}`, http.StatusBadRequest)
				return
			}
			if err := validateDealAllowlists(in.AdvertiserIDs, in.PlacementIDs, in.StartDate, in.EndDate); err != nil {
				http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
				return
			}
			id, err := store.CreateDeal(r.Context(), claims.AccountID, in)
			if errors.Is(err, errDealPublisherNotOwned) {
				http.Error(w, `{"error":"forbidden: publisher not in your account"}`, http.StatusForbidden)
				return
			}
			if errors.Is(err, errDealPlacementNotOwned) {
				http.Error(w, `{"error":"forbidden: a placement in the allowlist is not in your account"}`, http.StatusForbidden)
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

// dealByIDHandler serves PATCH /v1/api/deals/{id} (deals:update): edit
// name/price or pause/resume. Tenant-scoped; publishes the deals cache
// invalidate so the exchange reflects the change sub-second.
func dealByIDHandler(store dealStore, bus events.EventBus, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if devTenantGuard(w, r, claims, nil) {
			return
		}
		if r.Method != http.MethodPatch {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		if !can(claims, "deals:update") {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, routes.APIDeals+"/")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		var in dealPatchInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
			return
		}
		if in.Name == nil && in.Price == nil && in.Status == nil &&
			in.AdvertiserIDs == nil && in.PlacementIDs == nil && in.GuaranteedVolume == nil &&
			in.StartDate == nil && in.EndDate == nil {
			http.Error(w, `{"error":"no fields to update"}`, http.StatusBadRequest)
			return
		}
		if in.Status != nil && *in.Status != "active" && *in.Status != "paused" {
			http.Error(w, `{"error":"status must be active or paused"}`, http.StatusBadRequest)
			return
		}
		if in.Price != nil && *in.Price < 0 {
			http.Error(w, `{"error":"price must be >= 0"}`, http.StatusBadRequest)
			return
		}
		if in.GuaranteedVolume != nil && *in.GuaranteedVolume < 0 {
			http.Error(w, `{"error":"guaranteed_volume must be >= 0"}`, http.StatusBadRequest)
			return
		}
		advIDs, plIDs, start, end := derefStrings(in.AdvertiserIDs), derefStrings(in.PlacementIDs), derefStr(in.StartDate), derefStr(in.EndDate)
		if err := validateDealAllowlists(advIDs, plIDs, start, end); err != nil {
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
			return
		}
		err := store.UpdateDeal(r.Context(), claims.AccountID, id, in)
		if errors.Is(err, errDealPlacementNotOwned) {
			http.Error(w, `{"error":"forbidden: a placement in the allowlist is not in your account"}`, http.StatusForbidden)
			return
		}
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, `{"error":"deal not found"}`, http.StatusNotFound)
			return
		}
		if err != nil {
			log.Error("deal update failed", "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		if bus != nil {
			_ = bus.Publish(r.Context(), events.SubjectCacheInvalidateDeals, []byte(`{"source":"gateway","id":"`+id+`"}`))
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "updated"})
	}
}

type pgDealStore struct{ db *sql.DB }

func (s pgDealStore) ListDeals(ctx context.Context, accountID string) ([]dealView, error) {
	if s.db == nil {
		return nil, sql.ErrConnDone
	}
	rows, closeFn, err := postgres.QueryTenantDB(ctx, s.db, accountID,
		`SELECT id::text, publisher_id::text, name, deal_type, COALESCE(price,0), status,
		        COALESCE(advertiser_ids::text[], '{}'), COALESCE(placement_ids::text[], '{}'),
		        COALESCE(guaranteed_volume, 0), start_date::text, end_date::text
		 FROM deals WHERE account_id = $1::uuid ORDER BY created_at DESC`, accountID)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	out := []dealView{}
	for rows.Next() {
		var d dealView
		var advIDs, plIDs pq.StringArray
		var start, end sql.NullString
		if err := rows.Scan(&d.ID, &d.PublisherID, &d.Name, &d.DealType, &d.Price, &d.Status,
			&advIDs, &plIDs, &d.GuaranteedVolume, &start, &end); err != nil {
			return nil, err
		}
		d.AdvertiserIDs, d.PlacementIDs = []string(advIDs), []string(plIDs)
		d.StartDate, d.EndDate = start.String, end.String
		out = append(out, d)
	}
	return out, rows.Err()
}

// verifyPlacementsOwned returns errDealPlacementNotOwned unless every id in
// plIDs is a placement under accountID. Empty list = nothing to check.
func verifyPlacementsOwned(ctx context.Context, tx *sql.Tx, accountID string, plIDs []string) error {
	if len(plIDs) == 0 {
		return nil
	}
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM placements WHERE id = ANY($1::uuid[]) AND account_id = $2::uuid`,
		pq.Array(plIDs), accountID).Scan(&n); err != nil {
		return err
	}
	if n != len(plIDs) {
		return errDealPlacementNotOwned
	}
	return nil
}

// dateArg turns a YYYY-MM-DD string into a query arg (nil for empty → NULL).
func dateArg(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s pgDealStore) UpdateDeal(ctx context.Context, accountID, id string, in dealPatchInput) error {
	if s.db == nil {
		return sql.ErrConnDone
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, accountID); err != nil {
		return fmt.Errorf("set tenant: %w", err)
	}
	// A supplied placement allowlist must reference the caller's placements.
	if in.PlacementIDs != nil {
		if err := verifyPlacementsOwned(ctx, tx, accountID, *in.PlacementIDs); err != nil {
			return err
		}
	}
	// COALESCE keeps unspecified fields (nil arg → SQL NULL → keep existing);
	// a supplied array/date REPLACES (an empty array clears the allowlist =
	// match-all). The account_id predicate is the tenant check.
	var advArg, plArg any
	if in.AdvertiserIDs != nil {
		advArg = pq.Array(*in.AdvertiserIDs)
	}
	if in.PlacementIDs != nil {
		plArg = pq.Array(*in.PlacementIDs)
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE deals SET
		   name = COALESCE($3, name),
		   price = COALESCE($4, price),
		   status = COALESCE($5, status),
		   advertiser_ids = COALESCE($6::uuid[], advertiser_ids),
		   placement_ids = COALESCE($7::uuid[], placement_ids),
		   guaranteed_volume = COALESCE($8, guaranteed_volume),
		   start_date = COALESCE($9::date, start_date),
		   end_date = COALESCE($10::date, end_date),
		   updated_at = now()
		 WHERE id = $1::uuid AND account_id = $2::uuid`,
		id, accountID, in.Name, in.Price, in.Status,
		advArg, plArg, in.GuaranteedVolume, dateArg(derefStr(in.StartDate)), dateArg(derefStr(in.EndDate)))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
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
	// Scope the whole tx to the caller's account so RLS admits their publisher +
	// the deal write under the NOBYPASSRLS app role (security #77).
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, accountID); err != nil {
		return "", err
	}

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
	if err := verifyPlacementsOwned(ctx, tx, accountID, in.PlacementIDs); err != nil {
		return "", err
	}
	cur := in.PriceCurrency
	if cur == "" {
		cur = "USD"
	}
	var volArg any
	if in.GuaranteedVolume > 0 {
		volArg = in.GuaranteedVolume
	}
	var id string
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO deals (publisher_id, account_id, name, deal_type, price, price_currency,
		                    advertiser_ids, placement_ids, guaranteed_volume, start_date, end_date,
		                    status, created_at, updated_at)
		 VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7::uuid[], $8::uuid[], $9, $10::date, $11::date,
		         'active', now(), now()) RETURNING id::text`,
		in.PublisherID, accountID, in.Name, in.DealType, in.Price, cur,
		pq.Array(in.AdvertiserIDs), pq.Array(in.PlacementIDs), volArg,
		dateArg(in.StartDate), dateArg(in.EndDate)).Scan(&id); err != nil {
		return "", err
	}
	return id, tx.Commit()
}
