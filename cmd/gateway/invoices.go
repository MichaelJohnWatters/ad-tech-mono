package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// invoiceView is one invoices row as the advertiser billing console lists it.
// Totals are DECIMAL dollars (already converted from micros at generation).
type invoiceView struct {
	ID          string  `json:"id"`
	PeriodStart string  `json:"period_start"`
	PeriodEnd   string  `json:"period_end"`
	Total       float64 `json:"total"`
	Currency    string  `json:"currency"`
	Status      string  `json:"status"`
	DueDate     string  `json:"due_date"`
}

// invoiceLineView is one invoice_line_items row (per campaign).
type invoiceLineView struct {
	CampaignID   string  `json:"campaign_id"`
	CampaignName string  `json:"campaign_name"`
	Impressions  int64   `json:"impressions"`
	Clicks       int64   `json:"clicks"`
	Conversions  int64   `json:"conversions"`
	Spend        float64 `json:"spend"`
	BidModel     string  `json:"bid_model"`
}

// invoiceDetail is the header + its per-campaign line items.
type invoiceDetail struct {
	invoiceView
	Lines []invoiceLineView `json:"lines"`
}

// invoicesResponse is the list payload.
type invoicesResponse struct {
	Invoices []invoiceView `json:"invoices"`
}

type invoiceStore interface {
	ListInvoices(ctx context.Context, accountID string) (invoicesResponse, error)
	// GetInvoice returns the invoice + its lines for the caller's account.
	// found=false when the id doesn't exist for this tenant (a cross-tenant id
	// looks identical to a missing one — both 404).
	GetInvoice(ctx context.Context, accountID, invoiceID string) (invoiceDetail, bool, error)
}

// invoicesHandler serves the advertiser's invoice history.
//
//	GET /v1/api/invoices       — billing:view — list (header rows only)
//	GET /v1/api/invoices/{id}  — billing:view — one invoice + its line items
//
// Read-only and tenant-scoped: every query filters by the caller's account_id,
// so a cross-tenant id returns 404. Invoices are written by the invoice-runner
// job, not through this API.
func invoicesHandler(store invoiceStore, log *slog.Logger) http.HandlerFunc {
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
		if !can(claims, "billing:view") {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")

		// Both the list route (routes.APIInvoices, no trailing slash) and the
		// detail subtree (routes.APIInvoiceDetail, trailing slash) register this
		// handler. The id is whatever follows the trailing-slash prefix; the bare
		// list path (and the prefix with nothing after it) yields "".
		var id string
		if strings.HasPrefix(r.URL.Path, routes.APIInvoiceDetail) {
			id = strings.Trim(strings.TrimPrefix(r.URL.Path, routes.APIInvoiceDetail), "/")
		}

		if id == "" {
			if devTenantGuard(w, r, claims, invoicesResponse{Invoices: []invoiceView{}}) {
				return
			}
			resp, err := store.ListInvoices(r.Context(), claims.AccountID)
			if err != nil {
				log.Error("invoice list failed", "error", err, "account_id", claims.AccountID)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		if devTenantGuard(w, r, claims, invoiceDetail{Lines: []invoiceLineView{}}) {
			return
		}
		detail, found, err := store.GetInvoice(r.Context(), claims.AccountID, id)
		if err != nil {
			log.Error("invoice get failed", "error", err, "account_id", claims.AccountID, "invoice_id", id)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		if !found {
			http.Error(w, `{"error":"invoice not found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(detail)
	}
}

type pgInvoiceStore struct{ db *sql.DB }

func (s pgInvoiceStore) ListInvoices(ctx context.Context, accountID string) (invoicesResponse, error) {
	out := invoicesResponse{Invoices: []invoiceView{}}
	if s.db == nil {
		return out, sql.ErrConnDone
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id::text, period_start::text, period_end::text, total, currency, status, due_date::text
		 FROM invoices WHERE account_id = $1::uuid ORDER BY period_end DESC, created_at DESC LIMIT 500`,
		accountID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var v invoiceView
		if err := rows.Scan(&v.ID, &v.PeriodStart, &v.PeriodEnd, &v.Total, &v.Currency, &v.Status, &v.DueDate); err != nil {
			return out, err
		}
		out.Invoices = append(out.Invoices, v)
	}
	return out, rows.Err()
}

func (s pgInvoiceStore) GetInvoice(ctx context.Context, accountID, invoiceID string) (invoiceDetail, bool, error) {
	var out invoiceDetail
	out.Lines = []invoiceLineView{}
	if s.db == nil {
		return out, false, sql.ErrConnDone
	}
	// Header, tenant-scoped. invoiceID is bound as a parameter cast to uuid; a
	// malformed id makes the cast fail (returned as an error, mapped to 500 by
	// the handler — the trailing-slash route only receives non-empty segments).
	err := s.db.QueryRowContext(ctx,
		`SELECT id::text, period_start::text, period_end::text, total, currency, status, due_date::text
		 FROM invoices WHERE id = $1::uuid AND account_id = $2::uuid`,
		invoiceID, accountID).Scan(&out.ID, &out.PeriodStart, &out.PeriodEnd, &out.Total, &out.Currency, &out.Status, &out.DueDate)
	if err == sql.ErrNoRows {
		return out, false, nil
	}
	if err != nil {
		return out, false, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT campaign_id, COALESCE(campaign_name,''), impressions, clicks, conversions, spend, COALESCE(bid_model,'')
		 FROM invoice_line_items WHERE invoice_id = $1::uuid AND account_id = $2::uuid
		 ORDER BY spend DESC`,
		invoiceID, accountID)
	if err != nil {
		return out, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var l invoiceLineView
		if err := rows.Scan(&l.CampaignID, &l.CampaignName, &l.Impressions, &l.Clicks, &l.Conversions, &l.Spend, &l.BidModel); err != nil {
			return out, false, err
		}
		out.Lines = append(out.Lines, l)
	}
	return out, true, rows.Err()
}
