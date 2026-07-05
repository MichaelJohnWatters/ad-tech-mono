package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
)

// revshareView is one publisher's revenue-share contract as the staff editor
// sees it. fee_pct is the platform's cut (%); the publisher keeps the rest.
type revshareView struct {
	PublisherID   string  `json:"publisher_id"`
	Name          string  `json:"name"`
	Domain        string  `json:"domain"`
	RevshareModel string  `json:"revshare_model"`
	FeePct        float64 `json:"fee_pct"`
}

type revsharePatch struct {
	RevshareModel string  `json:"revshare_model"`
	FeePct        float64 `json:"fee_pct"`
}

var validRevshareModels = map[string]bool{
	"fixed": true, "tiered": true, "guaranteed_minimum": true, "deal_type": true, "hybrid": true,
}

type revshareStore interface {
	ListRevshare(ctx context.Context) ([]revshareView, error)
	// UpdateRevshare sets a publisher's model + fee; sql.ErrNoRows if unknown.
	UpdateRevshare(ctx context.Context, publisherID string, in revsharePatch) error
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
			if in.FeePct < 0 || in.FeePct > 100 {
				http.Error(w, `{"error":"fee_pct must be 0-100"}`, http.StatusBadRequest)
				return
			}
			err := store.UpdateRevshare(r.Context(), id, in)
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
			log.Info("revshare updated", "publisher_id", id, "model", in.RevshareModel, "fee_pct", in.FeePct, "actor", claims.UserID)
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
	rows, err := s.db.QueryContext(ctx,
		`SELECT id::text, name, domain, revshare_model,
		        COALESCE((revshare_config->>'fee_pct')::float8, 20)
		 FROM publishers WHERE status != 'archived' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []revshareView{}
	for rows.Next() {
		var v revshareView
		if err := rows.Scan(&v.PublisherID, &v.Name, &v.Domain, &v.RevshareModel, &v.FeePct); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s pgRevshareStore) UpdateRevshare(ctx context.Context, publisherID string, in revsharePatch) error {
	if s.db == nil {
		return sql.ErrConnDone
	}
	// jsonb_set-free: rebuild the fee_pct object. Preserving other config keys
	// isn't needed today (fixed model only carries fee_pct); revisit if tiered
	// config editing lands.
	cfg := fmt.Sprintf(`{"fee_pct": %g}`, in.FeePct)
	res, err := s.db.ExecContext(ctx,
		`UPDATE publishers SET revshare_model = $2, revshare_config = $3::jsonb, updated_at = now()
		 WHERE id = $1::uuid`,
		publisherID, in.RevshareModel, cfg)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

