package main

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"

	arapg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/ara/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
)

// araReportsHandler serves GET /v1/api/ara/reports — the advertiser's Privacy
// Sandbox ARA reports (the reporting-only overlay). Account-scoped: a caller sees
// only their own account's rows (RLS + the tenant GUC in the store). This stream
// is deliberately noised / aggregated / delayed and is NEVER billed — the
// response says so explicitly so it isn't mistaken for the exact conversions.
func araReportsHandler(gwDB *sql.DB, log *slog.Logger) http.HandlerFunc {
	store := arapg.New(gwDB)
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}

		reports, err := store.ListReports(r.Context(), claims.AccountID, 200)
		if err != nil {
			log.Error("ara reports list failed", "account", claims.AccountID, "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		summary, err := store.SummaryForAccount(r.Context(), claims.AccountID)
		if err != nil {
			log.Error("ara summary failed", "account", claims.AccountID, "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"note":    "Privacy Sandbox ARA is a reporting-only overlay: noised, aggregated and delayed by design, and never billed. Kept separate from the exact conversions stream.",
			"summary": summary,
			"reports": reports,
		})
	}
}
