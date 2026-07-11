package main

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
)

// account is the shape the staff impersonation picker consumes.
type account struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	Competitor bool   `json:"competitor"` // advertiser backed by a competitor DSP — simulated external demand, shown distinctly
}

// accountsListHandler lists advertiser + publisher accounts for the staff
// impersonation picker. GET only, read-only. Permission is gated by the
// RequirePermission("support:read") wrapper at registration; this handler only
// serves platform staff (advertisers/publishers can't reach it), so no tenant
// filter is applied — that's the point: staff pick any account to view as.
func accountsListHandler(db *sql.DB, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		if db == nil {
			http.Error(w, `{"error":"database unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		rows, err := db.QueryContext(r.Context(), `
SELECT a.id::text, a.name, a.type, COALESCE(d.profile_type = 'competitor', false) AS competitor
FROM accounts a
LEFT JOIN dsps d ON d.id = a.dsp_id
WHERE a.type IN ('advertiser', 'publisher') AND a.status != 'archived'
ORDER BY competitor, a.type, a.name`)
		if err != nil {
			log.Error("accounts list query failed", "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		out := []account{}
		for rows.Next() {
			var a account
			if err := rows.Scan(&a.ID, &a.Name, &a.Type, &a.Competitor); err != nil {
				log.Error("accounts list scan failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			out = append(out, a)
		}
		json.NewEncoder(w).Encode(out)
	}
}
