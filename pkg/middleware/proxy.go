package middleware

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
)

// ReverseProxy creates a simple reverse proxy handler that forwards requests
// to a backend service. Used by the gateway to proxy API calls to internal services.
func ReverseProxy(target string, log *slog.Logger) http.Handler {
	client := &http.Client{Timeout: 30 * time.Second}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Build upstream URL
		upstreamURL := target + r.URL.Path
		if r.URL.RawQuery != "" {
			upstreamURL += "?" + r.URL.RawQuery
		}

		// Create upstream request
		upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, r.Body)
		if err != nil {
			log.Error("proxy: failed to create request", "error", err)
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}

		// Copy headers
		for k, vv := range r.Header {
			for _, v := range vv {
				upstreamReq.Header.Add(k, v)
			}
		}

		// Inject account_id from auth claims for multi-tenancy
		claims := ClaimsFromContext(r.Context())
		if claims != nil {
			upstreamReq.Header.Set(constants.HeaderAccountID, claims.AccountID)
			upstreamReq.Header.Set(constants.HeaderUserID, claims.UserID)
		}

		// Forward
		start := time.Now()
		resp, err := client.Do(upstreamReq)
		if err != nil {
			log.Error("proxy: upstream failed", "target", target, "error", err)
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		// Copy response headers
		for k, vv := range resp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)

		log.Debug("proxy",
			"method", r.Method,
			"path", r.URL.Path,
			"target", target,
			"status", resp.StatusCode,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// StripPrefix removes a path prefix before proxying.
// E.g. StripPrefix("/v1/api/campaigns", proxy) strips "/v1/api/campaigns"
// so "/v1/api/campaigns/li-001" becomes "/li-001" at the upstream.
func StripPrefix(prefix string, handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = strings.TrimPrefix(r.URL.Path, prefix)
		if r.URL.Path == "" {
			r.URL.Path = "/"
		}
		handler.ServeHTTP(w, r)
	})
}
