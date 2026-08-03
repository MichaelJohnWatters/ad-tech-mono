package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/reporting"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

// channelBreakdownHandler serves the platform-wide per-channel activity breakdown:
// impressions + cost grouped by channel over the last 7 days, with NO account
// filter (all tenants). Staff-gated at the gateway before it's proxied here — this
// endpoint is the internal, unscoped read.
func channelBreakdownHandler(engine *reporting.QueryEngine, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		to := time.Now()
		from := to.Add(-7 * 24 * time.Hour)
		res, err := engine.Query(r.Context(), analytics.QueryParams{
			Table:      "impressions",
			Metrics:    []string{"count", "sum_cost"},
			Dimensions: []string{"channel"},
			TimeFrom:   from,
			TimeTo:     to,
			OrderBy:    "count",
			OrderDir:   "desc",
		})
		if err != nil {
			log.Error("channel breakdown query failed", "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
	}
}
