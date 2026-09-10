package main

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/audit"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
)

// residencyRe validates a data-residency region token (e.g. us-east-1, eu,
// eu-west-1) — a conservative charset so the value is safe to store/display.
var residencyRe = regexp.MustCompile(`^[a-z0-9-]{2,32}$`)

// account is the shape the staff impersonation picker consumes.
type account struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	Residency  string `json:"residency_region"`
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
SELECT a.id::text, a.name, a.type, a.residency_region, COALESCE(d.profile_type = 'competitor', false) AS competitor
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
			if err := rows.Scan(&a.ID, &a.Name, &a.Type, &a.Residency, &a.Competitor); err != nil {
				log.Error("accounts list scan failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			out = append(out, a)
		}
		json.NewEncoder(w).Encode(out)
	}
}

// setAccountResidencyHandler lets platform staff pin an account's data-residency
// region (accounts.residency_region). PUT {account_id, residency_region}. Gated
// by RequirePermission("support:update") + staff-only reach at registration; the
// change takes effect on the account's next login (the region rides the JWT).
// accounts has no RLS (tenant root), so the bare UPDATE is correct under adtech_app.
func setAccountResidencyHandler(db *sql.DB, auditDB *sql.DB, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		if r.Method != http.MethodPut {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		if db == nil {
			http.Error(w, `{"error":"database unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		var in struct {
			AccountID string `json:"account_id"`
			Region    string `json:"residency_region"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
			http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
			return
		}
		if !uuidRe.MatchString(in.AccountID) || !residencyRe.MatchString(in.Region) {
			http.Error(w, `{"error":"invalid account_id or residency_region"}`, http.StatusBadRequest)
			return
		}
		res, err := db.ExecContext(r.Context(),
			`UPDATE accounts SET residency_region = $2, updated_at = now() WHERE id = $1::uuid`, in.AccountID, in.Region)
		if err != nil {
			log.Error("set residency failed", "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			http.Error(w, `{"error":"account not found"}`, http.StatusNotFound)
			return
		}
		actor := "user:unknown"
		if claims := middleware.ClaimsFromContext(r.Context()); claims != nil {
			actor = "user:" + claims.UserID
		}
		// Compliance-relevant (changes where the account's data is served) — audit it.
		_ = audit.Log(r.Context(), auditDB, audit.Entry{
			AccountID:    in.AccountID,
			ActorID:      actor,
			Action:       "account:set_residency",
			ResourceType: "account",
			ResourceID:   in.AccountID,
			Reason:       in.Region,
		})
		log.Info("account residency updated", "account", in.AccountID, "region", in.Region)
		_ = json.NewEncoder(w).Encode(map[string]string{"account_id": in.AccountID, "residency_region": in.Region})
	}
}
