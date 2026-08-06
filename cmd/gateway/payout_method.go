package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/audit"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
)

// validPayoutMethodTypes are the destination kinds a publisher can configure.
// Each type has its own required detail field (see validatePayoutMethodInput).
var validPayoutMethodTypes = map[string]bool{
	"bank_transfer": true, "paypal": true, "payoneer": true, "wire": true,
}

// payoutMethodInput is the PUT body. details holds the raw sensitive
// destination fields (account_number / iban / paypal_email / …) keyed by name;
// they are stored but never returned. Only fields the caller actually changed
// need be sent — on read the API masks details, so the UI blanks the inputs and
// re-sends only what the operator retyped.
type payoutMethodInput struct {
	MethodType         string            `json:"method_type"`
	DisplayName        string            `json:"display_name"`
	Details            map[string]string `json:"details"`
	MinimumPayoutCents int64             `json:"minimum_payout_cents"`
	Currency           string            `json:"currency"`
}

// payoutMethodView is the READ shape. It deliberately has no raw details — only
// MaskedTail (a "•••• 6789" / "j***@example.com" style hint). Configured is
// false when the publisher has no method yet, so the UI can render an empty
// form instead of guessing from zero values.
type payoutMethodView struct {
	Configured         bool   `json:"configured"`
	MethodType         string `json:"method_type,omitempty"`
	DisplayName        string `json:"display_name,omitempty"`
	MaskedTail         string `json:"masked_tail,omitempty"`
	MinimumPayoutCents int64  `json:"minimum_payout_cents"`
	Currency           string `json:"currency"`
	Status             string `json:"status,omitempty"`
}

type payoutMethod struct {
	MethodType         string
	DisplayName        string
	Last4              string
	MinimumPayoutCents int64
	Currency           string
	Status             string
}

// masked renders the read view — the raw details never leave the store.
func (m payoutMethod) masked() payoutMethodView {
	return payoutMethodView{
		Configured:         true,
		MethodType:         m.MethodType,
		DisplayName:        m.DisplayName,
		MaskedTail:         maskTail(m.MethodType, m.Last4),
		MinimumPayoutCents: m.MinimumPayoutCents,
		Currency:           m.Currency,
		Status:             m.Status,
	}
}

// detailField is the raw sensitive field a given method type keys off — the one
// we validate as required and derive last4 from.
func detailField(methodType string) string {
	switch methodType {
	case "paypal":
		return "paypal_email"
	case "bank_transfer":
		return "account_number"
	default: // payoneer, wire
		return "iban"
	}
}

// validatePayoutMethodInput enforces the money-touching bounds and the
// per-type required detail field. Returns the raw destination value that last4
// is derived from (empty when the caller is only editing threshold/display and
// left details untouched — the store then keeps the stored value).
func validatePayoutMethodInput(in *payoutMethodInput) (string, error) {
	if !validPayoutMethodTypes[in.MethodType] {
		return "", errBadField("method_type must be bank_transfer, paypal, payoneer or wire")
	}
	if in.MinimumPayoutCents < 0 {
		return "", errBadField("minimum_payout_cents must be >= 0")
	}
	raw := strings.TrimSpace(in.Details[detailField(in.MethodType)])
	// A brand-new method must carry its destination; an edit that omits details
	// (masked round-trip) keeps the stored value, handled in the store.
	return raw, nil
}

type payoutMethodStore interface {
	// Get returns the caller's configured method; found=false when none exists.
	Get(ctx context.Context, accountID string) (payoutMethod, bool, error)
	// Upsert writes the single active method for the account, deriving+storing
	// last4 from the type's raw detail field. rawDetail is "" when the caller
	// left details untouched — implementations must then require an existing row
	// (a first-time config without a destination is rejected as ErrNoRows).
	Upsert(ctx context.Context, accountID string, in payoutMethodInput, rawDetail string) (payoutMethod, error)
}

// payoutMethodHandler serves a publisher's payout destination config.
//
//	GET /v1/api/payout-method — earnings:view  — masked read (never raw details)
//	PUT /v1/api/payout-method — earnings:manage — upsert the single active method
//
// Tenant-scoped by claims.AccountID throughout. The raw destination fields are
// stored but never returned — reads expose only a masked tail.
func payoutMethodHandler(store payoutMethodStore, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")

		// Unconfigured is the empty-form shape (also the dev-bypass GET response).
		empty := payoutMethodView{Configured: false, Currency: "USD"}
		if devTenantGuard(w, r, claims, empty) {
			return
		}

		switch r.Method {
		case http.MethodGet:
			if !can(claims, "earnings:view") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			m, found, err := store.Get(r.Context(), claims.AccountID)
			if err != nil {
				log.Error("payout method get failed", "error", err, "account_id", claims.AccountID)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			if !found {
				_ = json.NewEncoder(w).Encode(empty)
				return
			}
			_ = json.NewEncoder(w).Encode(m.masked())

		case http.MethodPut:
			if !can(claims, "earnings:manage") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			var in payoutMethodInput
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
				return
			}
			if in.Currency == "" {
				in.Currency = "USD"
			}
			raw, err := validatePayoutMethodInput(&in)
			if err != nil {
				http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
				return
			}
			m, err := store.Upsert(r.Context(), claims.AccountID, in, raw)
			if err == errPayoutDetailRequired {
				http.Error(w, `{"error":"`+detailField(in.MethodType)+` is required for `+in.MethodType+`"}`, http.StatusBadRequest)
				return
			}
			if err != nil {
				log.Error("payout method upsert failed", "error", err, "account_id", claims.AccountID)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			// Money-destination change — always leave an audit trail. The masked
			// tail (not the raw destination) is what lands in the log.
			_ = audit.Log(r.Context(), auditDBFrom(store), audit.Entry{
				AccountID:    claims.AccountID,
				ActorID:      claims.UserID,
				Action:       "payout_method:update",
				ResourceType: "payout_method",
				ResourceID:   claims.AccountID,
				Changes: map[string]any{
					"method_type": m.MethodType, "display_name": m.DisplayName,
					"masked_tail":          maskTail(m.MethodType, m.Last4),
					"minimum_payout_cents": m.MinimumPayoutCents, "currency": m.Currency,
				},
			})
			log.Info("payout method updated", "account_id", claims.AccountID,
				"method_type", m.MethodType, "min_payout_cents", m.MinimumPayoutCents,
				"currency", m.Currency, "actor", claims.UserID)
			_ = json.NewEncoder(w).Encode(m.masked())

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

// auditDBFrom pulls the *sql.DB from the pg store so the handler can write the
// audit row on the same connection; fakes return nil and audit.Log no-ops.
func auditDBFrom(store payoutMethodStore) *sql.DB {
	if s, ok := store.(pgPayoutMethodStore); ok {
		return s.db
	}
	return nil
}

// errPayoutDetailRequired is returned when a first-time config arrives without
// its destination field (nothing stored to fall back on). Mapped to 400.
var errPayoutDetailRequired = badFieldErr{"payout detail required"}

// maskTail renders a masked hint from the cached last4. Email destinations
// (PayPal) get a "j***@example.com" style; account/IBAN tails get "•••• 6789".
func maskTail(methodType, last4 string) string {
	if last4 == "" {
		return ""
	}
	if methodType == "paypal" {
		// last4 holds the email for paypal; mask the local part.
		at := strings.IndexByte(last4, '@')
		if at <= 1 {
			return last4
		}
		return last4[:1] + "***" + last4[at:]
	}
	return "•••• " + last4
}

// tailOf derives the cached last4/hint stored for a raw destination value.
// PayPal keeps the whole email (masked at render); everything else keeps the
// last 4 chars of the account/IBAN.
func tailOf(methodType, raw string) string {
	if methodType == "paypal" {
		return raw
	}
	if len(raw) <= 4 {
		return raw
	}
	return raw[len(raw)-4:]
}

type pgPayoutMethodStore struct{ db *sql.DB }

func (s pgPayoutMethodStore) Get(ctx context.Context, accountID string) (payoutMethod, bool, error) {
	var m payoutMethod
	if s.db == nil {
		return m, false, sql.ErrConnDone
	}
	var last4, display sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT method_type, COALESCE(display_name,''), last4, minimum_payout_cents, currency, status
		 FROM payout_methods WHERE account_id = $1::uuid AND status = 'active'`,
		accountID).Scan(&m.MethodType, &display, &last4, &m.MinimumPayoutCents, &m.Currency, &m.Status)
	if err == sql.ErrNoRows {
		return m, false, nil
	}
	if err != nil {
		return m, false, err
	}
	m.DisplayName, m.Last4 = display.String, last4.String
	return m, true, nil
}

func (s pgPayoutMethodStore) Upsert(ctx context.Context, accountID string, in payoutMethodInput, rawDetail string) (payoutMethod, error) {
	var out payoutMethod
	if s.db == nil {
		return out, sql.ErrConnDone
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	// RLS tenant GUC so the write is admitted and scoped to accountID.
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, accountID); err != nil {
		return out, err
	}

	// details is stored only when the caller supplied a new destination
	// (rawDetail != ""); an edit that leaves details blank keeps the stored
	// details + last4. A first-time config with no destination is rejected.
	if rawDetail == "" {
		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT true FROM payout_methods WHERE account_id = $1::uuid AND status = 'active'`,
			accountID).Scan(&exists); err == sql.ErrNoRows {
			return out, errPayoutDetailRequired
		} else if err != nil {
			return out, err
		}
		// Update everything except details/last4 (unchanged destination).
		if err := tx.QueryRowContext(ctx,
			`UPDATE payout_methods
			 SET method_type = $2, display_name = NULLIF($3,''),
			     minimum_payout_cents = $4, currency = $5, updated_at = now()
			 WHERE account_id = $1::uuid AND status = 'active'
			 RETURNING method_type, COALESCE(display_name,''), last4, minimum_payout_cents, currency, status`,
			accountID, in.MethodType, in.DisplayName, in.MinimumPayoutCents, in.Currency).
			Scan(&out.MethodType, &out.DisplayName, &out.Last4, &out.MinimumPayoutCents, &out.Currency, &out.Status); err != nil {
			return out, err
		}
		return out, tx.Commit()
	}

	detailsJSON, err := json.Marshal(in.Details)
	if err != nil {
		return out, err
	}
	last4 := tailOf(in.MethodType, rawDetail)
	// Single active row per account (partial unique index on status='active').
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO payout_methods
		     (account_id, method_type, display_name, details, last4, minimum_payout_cents, currency, status)
		 VALUES ($1::uuid, $2, NULLIF($3,''), $4::jsonb, $5, $6, $7, 'active')
		 ON CONFLICT (account_id) WHERE status = 'active' DO UPDATE
		   SET method_type = EXCLUDED.method_type, display_name = EXCLUDED.display_name,
		       details = EXCLUDED.details, last4 = EXCLUDED.last4,
		       minimum_payout_cents = EXCLUDED.minimum_payout_cents,
		       currency = EXCLUDED.currency, updated_at = now()
		 RETURNING method_type, COALESCE(display_name,''), last4, minimum_payout_cents, currency, status`,
		accountID, in.MethodType, in.DisplayName, string(detailsJSON), last4, in.MinimumPayoutCents, in.Currency).
		Scan(&out.MethodType, &out.DisplayName, &out.Last4, &out.MinimumPayoutCents, &out.Currency, &out.Status); err != nil {
		return out, err
	}
	return out, tx.Commit()
}
