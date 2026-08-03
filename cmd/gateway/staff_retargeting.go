package main

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

// staffRetargetingRow is one advertiser's retargeting audience as the staff
// oversight console renders it (platform-wide).
type staffRetargetingRow struct {
	Advertiser  string    `json:"advertiser"`
	Segment     string    `json:"segment"`
	Tag         string    `json:"tag"`
	WindowDays  int       `json:"window_days"`
	Members     int       `json:"members"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// staffRetargetingHandler serves GET /v1/api/staff/retargeting — every
// advertiser's retargeting audiences with their live (non-expired) enrollment,
// so platform staff can see who's retargeting and how big each pool is. Staff
// only (support:read); reads cross-tenant via the platform hatch.
func staffRetargetingHandler(db *sql.DB, log *slog.Logger) http.HandlerFunc {
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
		if !can(claims, "support:read") {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		if db == nil {
			http.Error(w, `{"error":"store unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")

		pg := postgres.NewFromDB(db)
		rows, closeRows, err := pg.QueryPlatform(r.Context(), `
SELECT acc.name, s.name, COALESCE(s.rule->>'tag', ''),
       COALESCE((s.rule->>'window_days')::int, 30), COALESCE(c.n, 0), s.updated_at
FROM audience_segments s
JOIN accounts acc ON acc.id = s.account_id
LEFT JOIN (
    SELECT segment_id, count(*) AS n FROM audience_segment_members
    WHERE expires_at IS NULL OR expires_at > now()
    GROUP BY segment_id
) c ON c.segment_id = s.id
WHERE s.type = 'retargeting'
ORDER BY COALESCE(c.n, 0) DESC, s.updated_at DESC
LIMIT 200`)
		if err != nil {
			log.Error("staff retargeting oversight query failed", "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		defer closeRows()

		out := []staffRetargetingRow{}
		for rows.Next() {
			var v staffRetargetingRow
			if err := rows.Scan(&v.Advertiser, &v.Segment, &v.Tag, &v.WindowDays, &v.Members, &v.UpdatedAt); err != nil {
				log.Error("staff retargeting scan failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			out = append(out, v)
		}
		_ = json.NewEncoder(w).Encode(out)
	}
}
