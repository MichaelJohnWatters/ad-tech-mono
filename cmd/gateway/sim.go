package main

import (
	"encoding/json"
	"net/http"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/simulator/request"
)

// simRealismHandler serves the consent + identity query-param encoding for a
// (consent regime, identity type) selection, built by pkg/simulator/request —
// the single source of truth. The web publisher-simulator fetches this instead
// of re-implementing the TCF / GPP / UID2 encoding in JS, so the browser path
// and the CLI encode privacy/identity signals identically.
//
//	GET /v1/sim/realism?consent=<regime>&identity=<identity>&seed=<seed>
//	→ {"query":"gdpr=1&consent=CP...&uid2=..."}
func simRealismHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	regime := request.Regime(q.Get("consent"))
	identity := request.Identity(q.Get("identity"))
	seed := q.Get("seed")
	if seed == "" {
		// Stable per-selection when the caller doesn't supply a seed, matching
		// the CLI's per-persona identity stability.
		seed = q.Get("consent") + "|" + q.Get("identity")
	}

	params := request.RealismParams(regime, identity, seed)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"query": params.Encode()})
}

// simPersonasHandler serves the simulator persona registry as JSON, so any UI
// can list the same personas the CLI exposes.
func simPersonasHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(request.Personas)
}
