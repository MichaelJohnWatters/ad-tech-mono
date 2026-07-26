package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/audit"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/billing"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

// revshareView is one publisher's revenue-share contract as the staff editor
// sees it. fee_pct is the platform's cut (%); the publisher keeps the rest.
// Tiers/guaranteed/deal-type configs feed the billing engine's Contract; which
// one applies depends on revshare_model.
type revshareView struct {
	PublisherID       string             `json:"publisher_id"`
	Name              string             `json:"name"`
	Domain            string             `json:"domain"`
	RevshareModel     string             `json:"revshare_model"`
	FeePct            float64            `json:"fee_pct"`
	Tiers             []billing.Tier     `json:"tiers,omitempty"`
	GuaranteedMinCPM  float64            `json:"guaranteed_min_cpm,omitempty"`
	DealTypeModifiers map[string]float64 `json:"deal_type_modifiers,omitempty"`
	PaymentTerms      string             `json:"payment_terms"`
}

type revsharePatch struct {
	RevshareModel     string             `json:"revshare_model"`
	FeePct            float64            `json:"fee_pct"`
	Tiers             []billing.Tier     `json:"tiers,omitempty"`
	GuaranteedMinCPM  float64            `json:"guaranteed_min_cpm,omitempty"`
	DealTypeModifiers map[string]float64 `json:"deal_type_modifiers,omitempty"`
	PaymentTerms      string             `json:"payment_terms,omitempty"`
}

var validRevshareModels = map[string]bool{
	"fixed": true, "tiered": true, "guaranteed_minimum": true, "deal_type": true, "hybrid": true,
}

var validPaymentTerms = map[string]bool{
	"prepay": true, "net_15": true, "net_30": true, "net_60": true, "net_90": true,
}

// validateRevsharePatch enforces the money-touching bounds: fees in 0–100,
// contiguous ascending tiers starting at 0, non-negative guaranteed minimum,
// sane deal-type adjustments, and a known payment term.
func validateRevsharePatch(in *revsharePatch) error {
	if in.FeePct < 0 || in.FeePct > 100 {
		return errBadField("fee_pct must be 0-100")
	}
	if in.GuaranteedMinCPM < 0 {
		return errBadField("guaranteed_min_cpm must be >= 0")
	}
	for dt, m := range in.DealTypeModifiers {
		if m < -100 || m > 100 {
			return errBadField("deal_type_modifiers[" + dt + "] must be -100..100")
		}
	}
	if in.PaymentTerms != "" && !validPaymentTerms[in.PaymentTerms] {
		return errBadField("payment_terms must be prepay, net_15, net_30, net_60 or net_90")
	}
	if len(in.Tiers) > 0 {
		sort.Slice(in.Tiers, func(i, j int) bool { return in.Tiers[i].MinImpressions < in.Tiers[j].MinImpressions })
		if in.Tiers[0].MinImpressions != 0 {
			return errBadField("tiers must start at min_impressions 0")
		}
		for i, t := range in.Tiers {
			if t.FeePct < 0 || t.FeePct > 100 {
				return errBadField("tier fee_pct must be 0-100")
			}
			last := i == len(in.Tiers)-1
			if !last {
				if t.MaxImpressions <= t.MinImpressions {
					return errBadField("tier max_impressions must exceed its min_impressions")
				}
				if t.MaxImpressions != in.Tiers[i+1].MinImpressions {
					return errBadField("tiers must be contiguous (each max = the next min)")
				}
			}
		}
	}
	return nil
}

type badFieldErr struct{ msg string }

func (e badFieldErr) Error() string { return e.msg }
func errBadField(m string) error    { return badFieldErr{m} }

type revshareStore interface {
	ListRevshare(ctx context.Context) ([]revshareView, error)
	// UpdateRevshare sets a publisher's model + full config + payment terms and
	// writes an audit entry; sql.ErrNoRows if the publisher is unknown.
	UpdateRevshare(ctx context.Context, publisherID, actor string, in revsharePatch) error
}

// revshareHandler is the staff revenue-share editor: GET lists every
// publisher's split (support:read), PATCH ?id= updates one (support:update).
// Platform-wide by design — rev share is a platform↔publisher commercial term
// staff owns, so it's permission-gated, not tenant-scoped. Every update
// publishes the billing-rates invalidate so reporting's ContractLoader
// re-reads the split within NATS RTT.
func revshareHandler(store revshareStore, bus events.EventBus, log *slog.Logger) http.HandlerFunc {
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
			rows, err := store.ListRevshare(r.Context())
			if err != nil {
				log.Error("revshare list failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(rows)

		case http.MethodPatch:
			if !can(claims, "support:update") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			id := strings.TrimSpace(r.URL.Query().Get("id"))
			if id == "" || !uuidRe.MatchString(id) {
				http.Error(w, `{"error":"id query param must be a publisher UUID"}`, http.StatusBadRequest)
				return
			}
			var in revsharePatch
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
				return
			}
			if in.RevshareModel == "" {
				in.RevshareModel = "fixed"
			}
			if !validRevshareModels[in.RevshareModel] {
				http.Error(w, `{"error":"revshare_model must be fixed, tiered, guaranteed_minimum, deal_type or hybrid"}`, http.StatusBadRequest)
				return
			}
			if err := validateRevsharePatch(&in); err != nil {
				http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
				return
			}
			err := store.UpdateRevshare(r.Context(), id, claims.UserID, in)
			if err == sql.ErrNoRows {
				http.Error(w, `{"error":"publisher not found"}`, http.StatusNotFound)
				return
			}
			if err != nil {
				log.Error("revshare update failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			if bus != nil {
				_ = bus.Publish(r.Context(), events.SubjectCacheInvalidateBillingRates,
					[]byte(`{"source":"gateway-revshare","id":"`+id+`"}`))
			}
			log.Info("revshare updated", "publisher_id", id, "model", in.RevshareModel,
				"fee_pct", in.FeePct, "tiers", len(in.Tiers), "payment_terms", in.PaymentTerms, "actor", claims.UserID)
			_ = json.NewEncoder(w).Encode(map[string]any{"publisher_id": id, "revshare_model": in.RevshareModel, "fee_pct": in.FeePct})

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

type pgRevshareStore struct{ db *sql.DB }

func (s pgRevshareStore) ListRevshare(ctx context.Context) ([]revshareView, error) {
	if s.db == nil {
		return nil, sql.ErrConnDone
	}
	// Staff-wide read (every publisher, no account filter) → platform hatch so
	// RLS admits all rows under the NOBYPASSRLS app role (security #77).
	rows, closeFn, err := postgres.NewFromDB(s.db).QueryPlatform(ctx,
		`SELECT id::text, name, domain, revshare_model,
		        COALESCE(revshare_config::text, '{}'), COALESCE(payment_terms, 'net_30')
		 FROM publishers WHERE status != 'archived' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	out := []revshareView{}
	for rows.Next() {
		var v revshareView
		var cfgJSON string
		if err := rows.Scan(&v.PublisherID, &v.Name, &v.Domain, &v.RevshareModel, &cfgJSON, &v.PaymentTerms); err != nil {
			return nil, err
		}
		var cfg struct {
			FeePct            float64            `json:"fee_pct"`
			Tiers             []billing.Tier     `json:"tiers"`
			GuaranteedMinCPM  float64            `json:"guaranteed_min_cpm"`
			DealTypeModifiers map[string]float64 `json:"deal_type_modifiers"`
		}
		_ = json.Unmarshal([]byte(cfgJSON), &cfg)
		if cfg.FeePct == 0 {
			cfg.FeePct = 20 // mirror the ContractLoader default
		}
		v.FeePct, v.Tiers, v.GuaranteedMinCPM, v.DealTypeModifiers = cfg.FeePct, cfg.Tiers, cfg.GuaranteedMinCPM, cfg.DealTypeModifiers
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s pgRevshareStore) UpdateRevshare(ctx context.Context, publisherID, actor string, in revsharePatch) error {
	if s.db == nil {
		return sql.ErrConnDone
	}
	// Store the full config in the exact shape the billing ContractLoader reads.
	cfg, err := json.Marshal(map[string]any{
		"fee_pct":             in.FeePct,
		"tiers":               in.Tiers,
		"guaranteed_min_cpm":  in.GuaranteedMinCPM,
		"deal_type_modifiers": in.DealTypeModifiers,
	})
	if err != nil {
		return err
	}
	// Staff cross-tenant write: resolve the publisher's account via the platform
	// hatch, then scope the UPDATE to it so RLS admits the write under the
	// NOBYPASSRLS app role (security #77).
	var pubAccount string
	if err := postgres.NewFromDB(s.db).QueryRowPlatform(ctx, func(row *sql.Row) error {
		return row.Scan(&pubAccount)
	}, "SELECT account_id::text FROM publishers WHERE id = $1::uuid", publisherID); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "SELECT set_config('app.current_account_id', $1, true)", pubAccount); err != nil {
		return err
	}
	// payment_terms only changes when supplied (empty = leave as-is).
	res, err := tx.ExecContext(ctx,
		`UPDATE publishers SET revshare_model = $2, revshare_config = $3::jsonb,
		        payment_terms = COALESCE(NULLIF($4, ''), payment_terms), updated_at = now()
		 WHERE id = $1::uuid`,
		publisherID, in.RevshareModel, string(cfg), in.PaymentTerms)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Money-touching change → audit trail (best-effort; the update already
	// committed, so a failed audit write is logged by the caller, not fatal).
	_ = audit.Log(ctx, s.db, audit.Entry{
		ActorID:      actor,
		Action:       "revshare:update",
		ResourceType: "publisher",
		ResourceID:   publisherID,
		Changes:      in,
	})
	return nil
}
