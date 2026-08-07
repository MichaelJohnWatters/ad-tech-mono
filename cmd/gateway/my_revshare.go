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

// myRevshareView is a publisher's OWN revenue-share terms — the tenant-scoped
// read behind /v1/api/my-revshare. It lets the publisher portal net out the
// platform fee (show net, not just gross, earnings) and surface the split to
// the publisher, without exposing the staff-only revshare editor.
type myRevshareView struct {
	PublisherID      string  `json:"publisher_id"`
	Name             string  `json:"name"`
	RevshareModel    string  `json:"revshare_model"`
	FeePct           float64 `json:"fee_pct"`
	GuaranteedMinCPM float64 `json:"guaranteed_min_cpm,omitempty"`
	PaymentTerms     string  `json:"payment_terms"`
}

type myRevshareStore interface {
	RevshareForAccount(ctx context.Context, accountID string) ([]myRevshareView, error)
}

// myRevshareHandler serves the caller publisher's own revshare terms (GET,
// earnings:view). Tenant-scoped: filters by the session account_id.
func myRevshareHandler(store myRevshareStore, log *slog.Logger) http.HandlerFunc {
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
		if devTenantGuard(w, r, claims, []myRevshareView{}) {
			return
		}
		rows, err := store.RevshareForAccount(r.Context(), claims.AccountID)
		if err != nil {
			log.Error("my-revshare query failed", "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(rows)
	}
}

type pgMyRevshareStore struct{ db *sql.DB }

func (s pgMyRevshareStore) RevshareForAccount(ctx context.Context, accountID string) ([]myRevshareView, error) {
	out := []myRevshareView{}
	if s.db == nil {
		return out, sql.ErrConnDone
	}
	// Tenant GUC must be set or RLS silently blanks the rows under the
	// NOBYPASSRLS app role (security #77) — same pattern as deals/quality.
	rows, closeFn, err := postgres.QueryTenantDB(ctx, s.db, accountID,
		`SELECT id::text, name, revshare_model,
		        COALESCE(revshare_config::text, '{}'), COALESCE(payment_terms, 'net_30')
		 FROM publishers WHERE account_id = $1::uuid AND status != 'archived' ORDER BY name`, accountID)
	if err != nil {
		return out, err
	}
	defer closeFn()
	for rows.Next() {
		var v myRevshareView
		var cfgJSON string
		if err := rows.Scan(&v.PublisherID, &v.Name, &v.RevshareModel, &cfgJSON, &v.PaymentTerms); err != nil {
			return out, err
		}
		var cfg struct {
			FeePct           float64 `json:"fee_pct"`
			GuaranteedMinCPM float64 `json:"guaranteed_min_cpm"`
		}
		_ = json.Unmarshal([]byte(cfgJSON), &cfg)
		if cfg.FeePct == 0 {
			cfg.FeePct = 20 // mirror the ContractLoader default fallback
		}
		v.FeePct, v.GuaranteedMinCPM = cfg.FeePct, cfg.GuaranteedMinCPM
		out = append(out, v)
	}
	return out, rows.Err()
}
