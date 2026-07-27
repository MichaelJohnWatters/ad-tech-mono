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

// topupMaxAmount caps a single topup. Arbitrary dev ceiling — protects
// against a fat-fingered 5000000 while the payment leg is fake and
// approves everything.
const topupMaxAmount = 10_000.0

// errTopupKeyReused is returned when an idempotency key is replayed with a
// different amount — that's not a retry, it's a bug in the caller, and
// silently returning the original topup would hide it. Mapped to 409.
var errTopupKeyReused = errors.New("idempotency key reused with different amount")

type topupInput struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
	// IdempotencyKey makes retries safe: the same key can never credit the
	// account twice. The portal generates one per form render, so a
	// double-click or an HTMX re-submit replays instead of re-charging.
	IdempotencyKey string `json:"idempotency_key"`
}

type topupResult struct {
	ID       string  `json:"id"`
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
	Status   string  `json:"status"`
	Balance  float64 `json:"balance"`
	// Duplicate marks an idempotent replay — the original topup is returned
	// and no new money moved.
	Duplicate bool `json:"duplicate,omitempty"`
}

type topupView struct {
	ID            string  `json:"id"`
	Amount        float64 `json:"amount"`
	Currency      string  `json:"currency"`
	Status        string  `json:"status"`
	PaymentMethod string  `json:"payment_method"`
	CreatedAt     string  `json:"created_at"`
}

// topupBalanceResponse is the GET payload: current balance + topup history +
// the account's billing mode (read-only for the advertiser — only staff change
// it, via the billing-terms editor). PaymentTerms is 'prepay' or 'invoiced';
// CreditLimit is the invoiced bidding headroom in dollars (0 for prepay).
type topupBalanceResponse struct {
	Balance      float64     `json:"balance"`
	Currency     string      `json:"currency"`
	PaymentTerms string      `json:"payment_terms"`
	CreditLimit  float64     `json:"credit_limit"`
	Topups       []topupView `json:"topups"`
}

type topupStore interface {
	// Topup credits the account. Must be idempotent on (accountID,
	// in.IdempotencyKey): a replay returns the original result with
	// Duplicate=true; a key reuse with a different amount returns
	// errTopupKeyReused.
	Topup(ctx context.Context, accountID, createdBy string, in topupInput) (topupResult, error)
	TopupHistory(ctx context.Context, accountID string) (topupBalanceResponse, error)
}

// topupHandler serves the advertiser prepay balance. GET (billing:view)
// returns balance + topup history; POST (billing:topup) credits the account.
//
// Money-touching: the payment approval is the dev/fake instant-success path
// (payment_method "dev"), but the accounting is real — the store writes the
// topup row, a double-entry ledger pair, and the balance upsert in one
// transaction, keyed by a client idempotency key. A real payment provider
// later replaces only the approval step.
func topupHandler(store topupStore, bus events.EventBus, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")

		if devTenantGuard(w, r, claims, topupBalanceResponse{Currency: "USD", PaymentTerms: "prepay", Topups: []topupView{}}) {
			return
		}

		switch r.Method {
		case http.MethodGet:
			if !can(claims, "billing:view") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			resp, err := store.TopupHistory(r.Context(), claims.AccountID)
			if err != nil {
				log.Error("topup history failed", "error", err, "account_id", claims.AccountID)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(resp)

		case http.MethodPost:
			if !can(claims, "billing:topup") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			var in topupInput
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
				return
			}
			if in.IdempotencyKey == "" {
				http.Error(w, `{"error":"idempotency_key required"}`, http.StatusBadRequest)
				return
			}
			if in.Amount <= 0 {
				http.Error(w, `{"error":"amount must be > 0"}`, http.StatusBadRequest)
				return
			}
			if in.Amount > topupMaxAmount {
				http.Error(w, fmt.Sprintf(`{"error":"amount exceeds maximum of %.0f"}`, topupMaxAmount), http.StatusBadRequest)
				return
			}
			if in.Currency == "" {
				in.Currency = "USD"
			}
			res, err := store.Topup(r.Context(), claims.AccountID, claims.UserID, in)
			if errors.Is(err, errTopupKeyReused) {
				http.Error(w, `{"error":"idempotency key already used with a different amount"}`, http.StatusConflict)
				return
			}
			if err != nil {
				log.Error("topup failed", "error", err, "account_id", claims.AccountID)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			status := http.StatusCreated
			if res.Duplicate {
				status = http.StatusOK
			} else {
				// New funds: ping the DSP balance caches so bidding
				// unblocks within NATS RTT instead of the next poll.
				if bus != nil {
					_ = bus.Publish(r.Context(), events.SubjectCacheInvalidateAdvertiserBalances,
						[]byte(`{"source":"gateway-topup","account_id":"`+claims.AccountID+`"}`))
				}
				// Money moved — always leave an operational trail.
				log.Info("topup credited",
					"account_id", claims.AccountID, "topup_id", res.ID,
					"amount", res.Amount, "currency", res.Currency, "balance", res.Balance)
			}
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(res)

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

type pgTopupStore struct{ db *sql.DB }

func (s pgTopupStore) Topup(ctx context.Context, accountID, createdBy string, in topupInput) (topupResult, error) {
	var out topupResult
	if s.db == nil {
		return out, sql.ErrConnDone
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, accountID); err != nil {
		return out, fmt.Errorf("set tenant: %w", err)
	}

	// created_by is best-effort: the dev-bypass admin's UserID isn't a
	// team_members UUID, and a topup must not fail on attribution.
	var createdByArg any
	if uuidRe.MatchString(createdBy) {
		createdByArg = createdBy
	}

	// Idempotency gate. ON CONFLICT DO NOTHING + no returned row = replay.
	var id string
	err = tx.QueryRowContext(ctx,
		`INSERT INTO topups (account_id, amount, currency, status, payment_method, idempotency_key, created_by)
		 VALUES ($1::uuid, $2, $3, 'succeeded', 'dev', $4, $5::uuid)
		 ON CONFLICT (account_id, idempotency_key) DO NOTHING
		 RETURNING id::text`,
		accountID, in.Amount, in.Currency, in.IdempotencyKey, createdByArg).Scan(&id)
	if err == sql.ErrNoRows {
		// Replay: return the original topup unchanged (no new ledger rows,
		// no balance change) — unless the caller changed the amount, which
		// is a bug we surface instead of masking.
		var orig topupResult
		if err := tx.QueryRowContext(ctx,
			`SELECT id::text, amount, currency, status FROM topups
			 WHERE account_id = $1::uuid AND idempotency_key = $2`,
			accountID, in.IdempotencyKey).Scan(&orig.ID, &orig.Amount, &orig.Currency, &orig.Status); err != nil {
			return out, err
		}
		if orig.Amount != in.Amount {
			return out, errTopupKeyReused
		}
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(balance, 0) FROM advertiser_balances WHERE account_id = $1::uuid`,
			accountID).Scan(&orig.Balance); err != nil && err != sql.ErrNoRows {
			return out, err
		}
		orig.Duplicate = true
		return orig, tx.Commit()
	}
	if err != nil {
		return out, err
	}

	// Double-entry pair: platform cash pays, the advertiser balance account
	// receives. reference_id links both rows back to the topup, so the
	// balance column is always re-derivable from the ledger.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO ledger_entries (account_code, entry_type, amount, currency, reference_type, reference_id)
		 VALUES ('platform:cash', 'debit', $1, $2, 'topup', $3),
		        ('advertiser:' || $4 || ':balance', 'credit', $1, $2, 'topup', $3)`,
		in.Amount, in.Currency, id, accountID); err != nil {
		return out, err
	}

	var balance float64
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO advertiser_balances (account_id, balance, currency, updated_at)
		 VALUES ($1::uuid, $2, $3, now())
		 ON CONFLICT (account_id) DO UPDATE
		   SET balance = advertiser_balances.balance + EXCLUDED.balance, updated_at = now()
		 RETURNING balance`,
		accountID, in.Amount, in.Currency).Scan(&balance); err != nil {
		return out, err
	}

	out = topupResult{ID: id, Amount: in.Amount, Currency: in.Currency, Status: "succeeded", Balance: balance}
	return out, tx.Commit()
}

func (s pgTopupStore) TopupHistory(ctx context.Context, accountID string) (topupBalanceResponse, error) {
	// Default posture for an account with no balance row yet: prepay, no credit.
	out := topupBalanceResponse{Currency: "USD", PaymentTerms: "prepay", Topups: []topupView{}}
	if s.db == nil {
		return out, sql.ErrConnDone
	}
	// Tenant-scoped reads → set the caller's account GUC so RLS admits their
	// balance + topups under the NOBYPASSRLS app role (security #77). Read-only
	// tx held open while scanning the topups.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, accountID); err != nil {
		return out, err
	}
	err = tx.QueryRowContext(ctx,
		`SELECT balance, currency, COALESCE(payment_terms, 'prepay'), COALESCE(credit_limit, 0)::float8
		 FROM advertiser_balances WHERE account_id = $1::uuid`,
		accountID).Scan(&out.Balance, &out.Currency, &out.PaymentTerms, &out.CreditLimit)
	if err != nil && err != sql.ErrNoRows {
		return out, err
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT id::text, amount, currency, status, payment_method, created_at::text
		 FROM topups WHERE account_id = $1::uuid ORDER BY created_at DESC LIMIT 100`,
		accountID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var t topupView
		if err := rows.Scan(&t.ID, &t.Amount, &t.Currency, &t.Status, &t.PaymentMethod, &t.CreatedAt); err != nil {
			return out, err
		}
		out.Topups = append(out.Topups, t)
	}
	return out, rows.Err()
}
