package main

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/attribution"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

// attributionResponse is the multi-touch breakdown for one conversion: its
// exposure chain with fractional credit apportioned under the requested model.
type attributionResponse struct {
	ConversionTraceID string                `json:"conversion_trace_id"`
	Model             string                `json:"model"`
	Revenue           float64               `json:"revenue"`
	Touchpoints       []attributionTPOutput `json:"touchpoints"`
}

type attributionTPOutput struct {
	TraceID        string  `json:"trace_id"`
	Type           string  `json:"type"`
	At             string  `json:"at"`
	CreditFraction float64 `json:"credit_fraction"`
	CreditRevenue  float64 `json:"credit_revenue"`
}

// attributionHandler serves GET /v1/reporting/attribution?conversion_trace=…&model=…
// — the read side of multi-touch attribution. The chain is stored model-agnostic;
// credit is apportioned here (pkg/attribution) so any model works without a
// re-write. Billing is unaffected (it settles last-touch).
func attributionHandler(store analytics.Store, log *slog.Logger) http.HandlerFunc {
	reader, _ := store.(analytics.AttributionReader)
	return func(w http.ResponseWriter, r *http.Request) {
		convTrace := r.URL.Query().Get("conversion_trace")
		if convTrace == "" {
			http.Error(w, "conversion_trace required", http.StatusBadRequest)
			return
		}
		model := r.URL.Query().Get("model")
		if model == "" {
			model = attribution.ModelLinear
		}
		if reader == nil {
			http.Error(w, "attribution reads not supported by this store", http.StatusNotImplemented)
			return
		}
		chain, err := reader.AttributionChain(r.Context(), convTrace)
		if err != nil {
			log.Error("attribution chain read failed", "conv_trace", convTrace, "error", err)
			http.Error(w, "read failed", http.StatusInternalServerError)
			return
		}

		resp := attributionResponse{ConversionTraceID: convTrace, Model: model}
		if len(chain) == 0 {
			writeJSON(w, resp)
			return
		}
		resp.Revenue = chain[0].ConversionRevenue
		tps := make([]attribution.Touchpoint, len(chain))
		for i, row := range chain {
			tps[i] = attribution.Touchpoint{TraceID: row.TouchpointTraceID, Type: row.TouchpointType, At: row.TouchpointAt}
		}
		credits := attribution.Apportion(tps, chain[0].ConversionAt, model)
		for _, c := range credits {
			resp.Touchpoints = append(resp.Touchpoints, attributionTPOutput{
				TraceID:        c.Touchpoint.TraceID,
				Type:           c.Touchpoint.Type,
				At:             c.Touchpoint.At.UTC().Format("2006-01-02T15:04:05Z"),
				CreditFraction: c.Fraction,
				CreditRevenue:  c.Fraction * resp.Revenue,
			})
		}
		writeJSON(w, resp)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
