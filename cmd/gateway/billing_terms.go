package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/audit"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
)

// billingTermsView is one advertiser account's billing posture as the staff
// editor sees it. payment_terms is 'prepay' (default; paid up front via topup)
// or 'invoiced' (postpay; may bid on credit up to credit_limit, billed monthly).
type billingTermsView struct {
	AccountID    string  `json:"account_id"`
	PaymentTerms string  `json:"payment_terms"`
	CreditLimit  float64 `json:"credit_limit"`
}

// validBillingTerms is the standardized two-value enum. Prepay is the special
// case credit_limit=0 in the DSP gate; invoiced extends the bidding headroom.
var validBillingTerms = map[string]bool{"prepay": true, "invoiced": true}

type billingTermsStore interface {
	// TermsFor returns the account's current terms. A never-topped-up account
	// (no advertiser_balances row) reads as prepay / 0 credit — the default.
	TermsFor(ctx context.Context, accountID string) (billingTermsView, error)
	// SetTerms UPSERTs the account's payment_terms + credit_limit (creating the
	// balance row at balance 0 if absent) and writes an audit entry.
	SetTerms(ctx context.Context, accountID, actor string, in billingTermsView) error
}

// billingTermsHandler is the staff advertiser-billing-terms editor. GET
// ?account_id= reads one account's terms (support:read); PUT sets them
// (support:update). Platform-wide by design — billing terms are a
// platform↔advertiser commercial setting staff owns, so it's permission-gated,
// not tenant-scoped (mirrors the revshare editor). Every PUT publishes the
// balance cache-invalidate so the DSP warm cache re-reads the new terms +
// credit_limit within NATS RTT instead of waiting for the 30s poll.
//
// Advertisers can never reach the PUT: it requires support:update, which
// customer roles lack. They see their mode read-only via the topup GET.
func billingTermsHandler(store billingTermsStore, bus events.EventBus, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")

		switch r.Method {
		case http.MethodGet:
			if !can(claims, "support:read") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			id := strings.TrimSpace(r.URL.Query().Get("account_id"))
			if id == "" || !uuidRe.MatchString(id) {
				http.Error(w, `{"error":"account_id query param must be an account UUID"}`, http.StatusBadRequest)
				return
			}
			v, err := store.TermsFor(r.Context(), id)
			if err != nil {
				log.Error("billing terms read failed", "error", err, "account_id", id)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(v)

		case http.MethodPut:
			if !can(claims, "support:update") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			var in billingTermsView
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
				return
			}
			in.AccountID = strings.TrimSpace(in.AccountID)
			if in.AccountID == "" || !uuidRe.MatchString(in.AccountID) {
				http.Error(w, `{"error":"account_id must be an account UUID"}`, http.StatusBadRequest)
				return
			}
			if !validBillingTerms[in.PaymentTerms] {
				http.Error(w, `{"error":"payment_terms must be prepay or invoiced"}`, http.StatusBadRequest)
				return
			}
			if in.CreditLimit < 0 {
				http.Error(w, `{"error":"credit_limit must be >= 0"}`, http.StatusBadRequest)
				return
			}
			// Prepay is credit_limit 0 by definition — normalize so a stray
			// limit on a prepay account can't leak into the gate formula.
			if in.PaymentTerms == "prepay" {
				in.CreditLimit = 0
			}
			if err := store.SetTerms(r.Context(), in.AccountID, claims.UserID, in); err != nil {
				log.Error("billing terms write failed", "error", err, "account_id", in.AccountID)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			// Ping the DSP balance caches so the new terms/credit_limit take
			// effect within NATS RTT, exactly as a topup does.
			if bus != nil {
				_ = bus.Publish(r.Context(), events.SubjectCacheInvalidateAdvertiserBalances,
					[]byte(`{"source":"gateway-billing-terms","account_id":"`+in.AccountID+`"}`))
			}
			log.Info("billing terms updated", "account_id", in.AccountID,
				"payment_terms", in.PaymentTerms, "credit_limit", in.CreditLimit, "actor", claims.UserID)
			_ = json.NewEncoder(w).Encode(in)

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

type pgBillingTermsStore struct{ db *sql.DB }

func (s pgBillingTermsStore) TermsFor(ctx context.Context, accountID string) (billingTermsView, error) {
	out := billingTermsView{AccountID: accountID, PaymentTerms: "prepay"}
	if s.db == nil {
		return out, sql.ErrConnDone
	}
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(payment_terms, 'prepay'), COALESCE(credit_limit, 0)::float8
		 FROM advertiser_balances WHERE account_id = $1::uuid`,
		accountID).Scan(&out.PaymentTerms, &out.CreditLimit)
	if err == sql.ErrNoRows {
		// Never topped up → default prepay, no credit. Not an error.
		return out, nil
	}
	if err != nil {
		return out, err
	}
	return out, nil
}

func (s pgBillingTermsStore) SetTerms(ctx context.Context, accountID, actor string, in billingTermsView) error {
	if s.db == nil {
		return sql.ErrConnDone
	}
	// UPSERT: create the balance row at balance 0 if the account never topped
	// up, else set just the terms + credit_limit (balance/currency untouched).
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO advertiser_balances (account_id, balance, currency, credit_limit, payment_terms, updated_at)
		 VALUES ($1::uuid, 0, 'USD', $2, $3, now())
		 ON CONFLICT (account_id) DO UPDATE
		   SET credit_limit = EXCLUDED.credit_limit, payment_terms = EXCLUDED.payment_terms, updated_at = now()`,
		accountID, in.CreditLimit, in.PaymentTerms); err != nil {
		return err
	}
	// Money-touching change → audit trail (best-effort; the upsert already
	// committed).
	_ = audit.Log(ctx, s.db, audit.Entry{
		ActorID:      actor,
		Action:       "billing_terms:update",
		ResourceType: "account",
		ResourceID:   accountID,
		Changes:      in,
	})
	return nil
}
