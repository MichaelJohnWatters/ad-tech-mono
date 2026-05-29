// Package middleware provides shared HTTP middleware for all services.
package middleware

import "net/http"

// CORS wraps an http.Handler with permissive CORS headers for local development.
// In production, Traefik handles CORS - this is only for dev tools
// that make cross-port browser requests (e.g. :8080 -> :8081).
func CORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
