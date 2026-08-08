package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// staffProxyGET is the shared skeleton for staff-console read proxies to an
// internal service (shading → DSP, channels → reporting): support:read gate,
// GET-only, 15s budget, 502 on service-down, body passed through verbatim.
// traceparent is forwarded so the staff request stays on one trace chain.
func staffProxyGET(targetURL, errLabel string, log *slog.Logger) http.HandlerFunc {
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
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
		if tp := r.Header.Get("traceparent"); tp != "" {
			req.Header.Set("traceparent", tp)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			log.Error("staff proxy failed", "target", errLabel, "error", err)
			http.Error(w, `{"error":"`+errLabel+` unavailable"}`, http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}
}

// staffShadingHandler serves GET /v1/api/staff/shading — the staff read-only
// view of the DSP's per-placement bid-shading state, proxied server-side to
// the DSP's internal GET /v1/dsp/shading so the staff page never needs
// internal service paths. The body is the DSP's
// map[placement_id]bidshading.PlacementStats verbatim: TotalBids / Wins /
// Losses / WinRate / AvgClearing / BelowFloor / Outbid. Read-only by design —
// shading is model-driven (pkg/bidshading), no knobs.
func staffShadingHandler(dspURL string, log *slog.Logger) http.HandlerFunc {
	return staffProxyGET(dspURL+routes.DSPShading, "dsp", log)
}
