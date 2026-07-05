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

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/lib/pq"
)

// Publisher direct-sold line items — the publisher's own commitments
// (sponsorships, guaranteed, preferred, house ads) that the publisher-adserver
// arbitrates against programmatic. The serving side (cmd/publisher-adserver +
// its warm cache) already existed; this is the missing publisher-facing CRUD.
// Reuses the deals RBAC perms (deals:read/create/update) — a direct line item
// is a publisher demand commitment, same as a deal.

var validPriorityTiers = map[string]bool{"sponsorship": true, "guaranteed": true, "preferred": true, "house": true}
var validPLIPacing = map[string]bool{"even": true, "asap": true}
var validPLIStatus = map[string]bool{"draft": true, "active": true, "paused": true, "ended": true}

type directLineItemInput struct {
	PublisherID          string   `json:"publisher_id"`
	Name                 string   `json:"name"`
	DemandSource         string   `json:"demand_source,omitempty"`
	PriorityTier         string   `json:"priority_tier"`
	PlacementIDs         []string `json:"placement_ids,omitempty"` // empty = any placement under the publisher
	ImpressionsCommitted int64    `json:"impressions_committed,omitempty"`
	DeliveryStart        string   `json:"delivery_start,omitempty"` // YYYY-MM-DD
	DeliveryEnd          string   `json:"delivery_end,omitempty"`
	CPM                  *float64 `json:"cpm,omitempty"`
	Currency             string   `json:"currency,omitempty"`
	PacingMode           string   `json:"pacing_mode,omitempty"`
}

type directLineItemView struct {
	ID                   string   `json:"id"`
	PublisherID          string   `json:"publisher_id"`
	Name                 string   `json:"name"`
	DemandSource         string   `json:"demand_source"`
	PriorityTier         string   `json:"priority_tier"`
	PlacementIDs         []string `json:"placement_ids"`
	ImpressionsCommitted int64    `json:"impressions_committed"`
	DeliveryStart        string   `json:"delivery_start,omitempty"`
	DeliveryEnd          string   `json:"delivery_end,omitempty"`
	CPM                  *float64 `json:"cpm,omitempty"`
	Currency             string   `json:"currency"`
	PacingMode           string   `json:"pacing_mode"`
	Status               string   `json:"status"`
}

type directLineItemPatchInput struct {
	Name                 *string   `json:"name,omitempty"`
	DemandSource         *string   `json:"demand_source,omitempty"`
	PriorityTier         *string   `json:"priority_tier,omitempty"`
	PlacementIDs         *[]string `json:"placement_ids,omitempty"`
	ImpressionsCommitted *int64    `json:"impressions_committed,omitempty"`
	DeliveryStart        *string   `json:"delivery_start,omitempty"`
	DeliveryEnd          *string   `json:"delivery_end,omitempty"`
	CPM                  *float64  `json:"cpm,omitempty"`
	PacingMode           *string   `json:"pacing_mode,omitempty"`
	Status               *string   `json:"status,omitempty"`
}

// validatePLIWindow checks placement-id UUIDs + the YYYY-MM-DD delivery window.
func validatePLIWindow(plIDs []string, start, end string) error {
	for _, id := range plIDs {
		if !uuidRe.MatchString(id) {
			return fmt.Errorf("placement_ids must be UUIDs, got %q", id)
		}
	}
	if !dealValidDate(start) || !dealValidDate(end) {
		return errors.New("delivery_start and delivery_end must be YYYY-MM-DD when set")
	}
	if start != "" && end != "" && end < start {
		return errors.New("delivery_end must be on or after delivery_start")
	}
	return nil
}

type directLineItemStore interface {
	ListDirectLineItems(ctx context.Context, accountID string) ([]directLineItemView, error)
	CreateDirectLineItem(ctx context.Context, accountID string, in directLineItemInput) (id string, err error)
	UpdateDirectLineItem(ctx context.Context, accountID, id string, in directLineItemPatchInput) error
}

// directLineItemsHandler lists (GET, deals:read) and creates (POST, deals:create)
// publisher direct-sold line items, scoped to the caller's account. A create
// publishes the publisher-line-items cache invalidate so the publisher-adserver
// picks it up sub-second.
func directLineItemsHandler(store directLineItemStore, bus events.EventBus, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if devTenantGuard(w, r, claims, []directLineItemView{}) {
			return
		}

		switch r.Method {
		case http.MethodGet:
			if !can(claims, "deals:read") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			items, err := store.ListDirectLineItems(r.Context(), claims.AccountID)
			if err != nil {
				log.Error("direct line items list failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(items)

		case http.MethodPost:
			if !can(claims, "deals:create") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			var in directLineItemInput
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
				return
			}
			if in.PublisherID == "" || in.Name == "" {
				http.Error(w, `{"error":"publisher_id and name required"}`, http.StatusBadRequest)
				return
			}
			if !validPriorityTiers[in.PriorityTier] {
				http.Error(w, `{"error":"priority_tier must be sponsorship, guaranteed, preferred or house"}`, http.StatusBadRequest)
				return
			}
			if in.PacingMode == "" {
				in.PacingMode = "even"
			}
			if !validPLIPacing[in.PacingMode] {
				http.Error(w, `{"error":"pacing_mode must be even or asap"}`, http.StatusBadRequest)
				return
			}
			if in.ImpressionsCommitted < 0 {
				http.Error(w, `{"error":"impressions_committed must be >= 0"}`, http.StatusBadRequest)
				return
			}
			if in.CPM != nil && *in.CPM < 0 {
				http.Error(w, `{"error":"cpm must be >= 0"}`, http.StatusBadRequest)
				return
			}
			if err := validatePLIWindow(in.PlacementIDs, in.DeliveryStart, in.DeliveryEnd); err != nil {
				http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
				return
			}
			id, err := store.CreateDirectLineItem(r.Context(), claims.AccountID, in)
			if errors.Is(err, errDealPublisherNotOwned) {
				http.Error(w, `{"error":"forbidden: publisher not in your account"}`, http.StatusForbidden)
				return
			}
			if errors.Is(err, errDealPlacementNotOwned) {
				http.Error(w, `{"error":"forbidden: a placement is not in your account"}`, http.StatusForbidden)
				return
			}
			if err != nil {
				log.Error("direct line item create failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			if bus != nil {
				_ = bus.Publish(r.Context(), events.SubjectCacheInvalidatePublisherLineItems, []byte(`{"source":"gateway","id":"`+id+`"}`))
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "created"})

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

// directLineItemByIDHandler serves PATCH /v1/api/direct-line-items/{id}
// (deals:update): edit fields or pause/resume/end. Tenant-scoped; publishes the
// publisher-line-items cache invalidate.
func directLineItemByIDHandler(store directLineItemStore, bus events.EventBus, log *slog.Logger) http.HandlerFunc {
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
		id := strings.TrimPrefix(r.URL.Path, routes.APIDirectLineItems+"/")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		var in directLineItemPatchInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
			return
		}
		if in.Name == nil && in.DemandSource == nil && in.PriorityTier == nil &&
			in.PlacementIDs == nil && in.ImpressionsCommitted == nil && in.DeliveryStart == nil &&
			in.DeliveryEnd == nil && in.CPM == nil && in.PacingMode == nil && in.Status == nil {
			http.Error(w, `{"error":"no fields to update"}`, http.StatusBadRequest)
			return
		}
		if in.PriorityTier != nil && !validPriorityTiers[*in.PriorityTier] {
			http.Error(w, `{"error":"priority_tier must be sponsorship, guaranteed, preferred or house"}`, http.StatusBadRequest)
			return
		}
		if in.PacingMode != nil && !validPLIPacing[*in.PacingMode] {
			http.Error(w, `{"error":"pacing_mode must be even or asap"}`, http.StatusBadRequest)
			return
		}
		if in.Status != nil && !validPLIStatus[*in.Status] {
			http.Error(w, `{"error":"status must be draft, active, paused or ended"}`, http.StatusBadRequest)
			return
		}
		if in.ImpressionsCommitted != nil && *in.ImpressionsCommitted < 0 {
			http.Error(w, `{"error":"impressions_committed must be >= 0"}`, http.StatusBadRequest)
			return
		}
		if in.CPM != nil && *in.CPM < 0 {
			http.Error(w, `{"error":"cpm must be >= 0"}`, http.StatusBadRequest)
			return
		}
		if err := validatePLIWindow(derefStrings(in.PlacementIDs), derefStr(in.DeliveryStart), derefStr(in.DeliveryEnd)); err != nil {
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
			return
		}
		err := store.UpdateDirectLineItem(r.Context(), claims.AccountID, id, in)
		if errors.Is(err, errDealPlacementNotOwned) {
			http.Error(w, `{"error":"forbidden: a placement is not in your account"}`, http.StatusForbidden)
			return
		}
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, `{"error":"direct line item not found"}`, http.StatusNotFound)
			return
		}
		if err != nil {
			log.Error("direct line item update failed", "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		if bus != nil {
			_ = bus.Publish(r.Context(), events.SubjectCacheInvalidatePublisherLineItems, []byte(`{"source":"gateway","id":"`+id+`"}`))
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "updated"})
	}
}

type pgDirectLineItemStore struct{ db *sql.DB }

func (s pgDirectLineItemStore) ListDirectLineItems(ctx context.Context, accountID string) ([]directLineItemView, error) {
	if s.db == nil {
		return nil, sql.ErrConnDone
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id::text, publisher_id::text, name, demand_source, priority_tier,
		        COALESCE(placement_ids::text[], '{}'), impressions_committed,
		        to_char(delivery_start, 'YYYY-MM-DD'), to_char(delivery_end, 'YYYY-MM-DD'),
		        cpm, currency, pacing_mode, status
		 FROM publisher_line_items WHERE account_id = $1::uuid ORDER BY created_at DESC`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []directLineItemView{}
	for rows.Next() {
		var v directLineItemView
		var plIDs pq.StringArray
		var start, end sql.NullString
		var cpm sql.NullFloat64
		if err := rows.Scan(&v.ID, &v.PublisherID, &v.Name, &v.DemandSource, &v.PriorityTier,
			&plIDs, &v.ImpressionsCommitted, &start, &end, &cpm, &v.Currency, &v.PacingMode, &v.Status); err != nil {
			return nil, err
		}
		v.PlacementIDs = []string(plIDs)
		v.DeliveryStart, v.DeliveryEnd = start.String, end.String
		if cpm.Valid {
			v.CPM = &cpm.Float64
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s pgDirectLineItemStore) CreateDirectLineItem(ctx context.Context, accountID string, in directLineItemInput) (string, error) {
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
	if err := verifyPlacementsOwned(ctx, tx, accountID, in.PlacementIDs); err != nil {
		return "", err
	}
	cur := in.Currency
	if cur == "" {
		cur = "USD"
	}
	var cpmArg any
	if in.CPM != nil {
		cpmArg = *in.CPM
	}
	var id string
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO publisher_line_items (account_id, publisher_id, name, demand_source, priority_tier,
		        placement_ids, impressions_committed, delivery_start, delivery_end, cpm, currency,
		        pacing_mode, status, created_at, updated_at)
		 VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6::uuid[], $7, $8::timestamptz, $9::timestamptz,
		         $10, $11, $12, 'active', now(), now()) RETURNING id::text`,
		accountID, in.PublisherID, in.Name, in.DemandSource, in.PriorityTier,
		pq.Array(in.PlacementIDs), in.ImpressionsCommitted, dateArg(in.DeliveryStart), dateArg(in.DeliveryEnd),
		cpmArg, cur, in.PacingMode).Scan(&id); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

func (s pgDirectLineItemStore) UpdateDirectLineItem(ctx context.Context, accountID, id string, in directLineItemPatchInput) error {
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
	if in.PlacementIDs != nil {
		if err := verifyPlacementsOwned(ctx, tx, accountID, *in.PlacementIDs); err != nil {
			return err
		}
	}
	var plArg any
	if in.PlacementIDs != nil {
		plArg = pq.Array(*in.PlacementIDs)
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE publisher_line_items SET
		   name = COALESCE($3, name),
		   demand_source = COALESCE($4, demand_source),
		   priority_tier = COALESCE($5, priority_tier),
		   placement_ids = COALESCE($6::uuid[], placement_ids),
		   impressions_committed = COALESCE($7, impressions_committed),
		   delivery_start = COALESCE($8::timestamptz, delivery_start),
		   delivery_end = COALESCE($9::timestamptz, delivery_end),
		   cpm = COALESCE($10, cpm),
		   pacing_mode = COALESCE($11, pacing_mode),
		   status = COALESCE($12, status),
		   updated_at = now()
		 WHERE id = $1::uuid AND account_id = $2::uuid`,
		id, accountID, in.Name, in.DemandSource, in.PriorityTier,
		plArg, in.ImpressionsCommitted, dateArg(derefStr(in.DeliveryStart)), dateArg(derefStr(in.DeliveryEnd)),
		in.CPM, in.PacingMode, in.Status)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}
