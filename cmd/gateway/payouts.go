package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

// payoutView is one payouts row as the publisher earnings console sees it.
type payoutView struct {
	ID          string  `json:"id"`
	PublisherID string  `json:"publisher_id"`
	Amount      float64 `json:"amount"`
	Currency    string  `json:"currency"`
	PlatformFee float64 `json:"platform_fee"`
	Status      string  `json:"status"`
	PeriodStart string  `json:"period_start"`
	PeriodEnd   string  `json:"period_end"`
}

// payoutsResponse is the earnings console payload: the payout rows plus a small
// pending/paid rollup so the UI can render headline numbers without a second
// query.
type payoutsResponse struct {
	Payouts      []payoutView `json:"payouts"`
	PendingCents int64        `json:"pending_cents"`
	PaidCents    int64        `json:"paid_cents"`
	Currency     string       `json:"currency"`
}

type payoutStore interface {
	ListPayouts(ctx context.Context, accountID string) (payoutsResponse, error)
}

// payoutsHandler serves a publisher's earnings/payout history (GET,
// earnings:view). Read-only and tenant-scoped — every query filters by the
// caller's account_id. Payouts are created by the billing settlement job, not
// through this API.
func payoutsHandler(store payoutStore, log *slog.Logger) http.HandlerFunc {
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
		if !can(claims, "earnings:view") {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if devTenantGuard(w, r, claims, payoutsResponse{Payouts: []payoutView{}, Currency: "USD"}) {
			return
		}
		resp, err := store.ListPayouts(r.Context(), claims.AccountID)
		if err != nil {
			log.Error("payout list failed", "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(resp)
	}
}

type pgPayoutStore struct{ db *sql.DB }

func (s pgPayoutStore) ListPayouts(ctx context.Context, accountID string) (payoutsResponse, error) {
	out := payoutsResponse{Payouts: []payoutView{}, Currency: "USD"}
	if s.db == nil {
		return out, sql.ErrConnDone
	}
	// Tenant GUC must be set or RLS silently blanks the rows under the
	// NOBYPASSRLS app role (security #77).
	rows, closeFn, err := postgres.QueryTenantDB(ctx, s.db, accountID,
		`SELECT id::text, publisher_id::text, amount, currency, platform_fee, status,
		        period_start::text, period_end::text
		 FROM payouts WHERE account_id = $1::uuid ORDER BY period_end DESC LIMIT 500`, accountID)
	if err != nil {
		return out, err
	}
	defer closeFn()
	for rows.Next() {
		var p payoutView
		if err := rows.Scan(&p.ID, &p.PublisherID, &p.Amount, &p.Currency, &p.PlatformFee, &p.Status, &p.PeriodStart, &p.PeriodEnd); err != nil {
			return out, err
		}
		cents := int64(p.Amount * 100)
		if p.Status == "paid" {
			out.PaidCents += cents
		} else {
			out.PendingCents += cents
		}
		if p.Currency != "" {
			out.Currency = p.Currency
		}
		out.Payouts = append(out.Payouts, p)
	}
	return out, rows.Err()
}
