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
		w.Header().Set(constants.HeaderCORSHeaders, constants.HeaderContentType+", traceparent, X-Dev-Slow-DSPs")
		// Expose dev-mode response headers so the pub sim UI can read
		// them from the fetch response. Custom headers are otherwise
		// invisible to JS even with permissive Access-Control-Allow-Origin.
		w.Header().Set("Access-Control-Expose-Headers", "X-Dev-Fraud-Blocked, X-Dev-Fraud-Reasons, X-Trace-Id")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
