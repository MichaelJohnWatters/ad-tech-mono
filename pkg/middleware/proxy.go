package middleware

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
)

// ReverseProxy creates a simple reverse proxy handler that forwards requests
// to a backend service. Used by the gateway to proxy API calls to internal services.
func ReverseProxy(target string, log *slog.Logger) http.Handler {
	// CheckRedirect returns ErrUseLastResponse so the gateway forwards
	// upstream 3xx responses verbatim instead of following them. Critical
	// for the tracker click flow where the 302 location points at an
	// external advertiser domain the gateway can't (and shouldn't) reach.
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

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

		// Propagate the W3C trace context (traceparent) to the upstream so the
		// downstream service continues THIS trace instead of forking a new one.
		// The gateway's tracing.HTTPMiddleware put the authoritative span
		// context in r.Context(); a request that arrived without a traceparent
		// (a browser fetch, a raw curl) would otherwise start a fresh trace at
		// each proxied hop — so the exchange's auction + AuctionWinEvent would
		// land under a different trace_id than the served impression, breaking
		// end-to-end continuity. Injecting from context makes the whole request
		// (auction → win → serve → impression) share one trace_id.
		tracing.InjectHTTP(r.Context(), upstreamReq)

		// Forward
		start := time.Now()
		resp, err := client.Do(upstreamReq)
		if err != nil {
			log.Error("proxy: upstream failed", "target", target, "error", err)
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		// Copy response headers, but strip any Access-Control-* headers
		// the upstream set. The gateway's own CORS middleware is the
		// authoritative source for these — copying upstream's values
		// would emit duplicates (Access-Control-Allow-Origin: *, *) and
		// strict CORS clients (notably the IMA SDK iframe) treat
		// concatenated wildcards as a mismatch with the request origin,
		// failing the resource-sharing check post-fetch.
		for k, vv := range resp.Header {
			if strings.HasPrefix(strings.ToLower(k), "access-control-") {
				continue
			}
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
		// A request for exactly the prefix strips to "" — leave it empty.
		// ReverseProxy targets already carry the full upstream base path
		// (e.g. dspURL + /v1/dsp/campaigns), so forcing "/" here produced
		// ".../v1/dsp/campaigns/" upstream, which ServeMux routes to the
		// by-ID subtree handler (empty id → 404) instead of the collection.
		r.URL.Path = strings.TrimPrefix(r.URL.Path, prefix)
		handler.ServeHTTP(w, r)
	})
}
