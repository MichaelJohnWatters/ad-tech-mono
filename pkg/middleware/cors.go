// Package middleware provides shared HTTP middleware for all services.
package middleware

import (
	"net/http"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
)

// CORS wraps an http.Handler with permissive CORS headers for local development.
// In production, Traefik handles CORS - this is only for dev tools
// that make cross-port browser requests (e.g. :8080 -> :8081).
func CORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(constants.HeaderCORSOrigin, "*")
		w.Header().Set(constants.HeaderCORSMethods, "GET, POST, PATCH, DELETE, OPTIONS")
		// Allow custom request headers used by dev tools (publisher
		// simulator) for trace propagation and one-shot scenario flags.
		// Browsers preflight POSTs with non-safelisted headers; without
		// this they'd reject the request before it ever hit the server.
		w.Header().Set(constants.HeaderCORSHeaders, constants.HeaderContentType+", traceparent, X-Dev-Slow-DSPs, X-API-Key")
		// Expose dev-mode response headers so the pub sim UI can read
		// them from the fetch response. Custom headers are otherwise
		// invisible to JS even with permissive Access-Control-Allow-Origin.
		// X-Adtech-Outcome (adserving.OutcomeHeader) lets the demo-site trace
		// panel show the auction outcome (fill/no-bid/house + advertiser + price)
		// uniformly across display JSON, VAST/DAAST XML and SSAI manifests.
		w.Header().Set("Access-Control-Expose-Headers", "X-Dev-Fraud-Blocked, X-Dev-Fraud-Reasons, X-Trace-Id, X-IAB-Viewable, X-Adtech-Outcome")
		// Chrome Private Network Access (CORS-RFC1918): when a page on
		// a public origin (e.g. https://imasdk.googleapis.com inside the
		// IMA SDK iframe) fetches a resource on a private/loopback
		// address, Chrome silently blocks the request unless the server
		// opts in via this header. Without it, IMA's VAST fetches to
		// http://localhost:8080 fail with status 0 / inner=6, surfaced
		// as the generic "problem requesting ads from the server" error.
		// See: https://developer.chrome.com/blog/private-network-access-preflight
		w.Header().Set("Access-Control-Allow-Private-Network", "true")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
