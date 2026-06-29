package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/secrets"
)

// apiKeyCtxKey is the request-context key for the validated Secret. Use
// SecretFromContext in downstream handlers to read which key authorised
// the call (for tenant scoping, audit logs, etc.).
type apiKeyCtxKey struct{}

// SecretFromContext returns the secrets row that authorised this request,
// or nil if AuthAPIKey middleware wasn't in the chain or no key was
// presented. Handlers MUST nil-check before using.
func SecretFromContext(ctx context.Context) *secrets.Secret {
	if s, ok := ctx.Value(apiKeyCtxKey{}).(*secrets.Secret); ok {
		return s
	}
	return nil
}

// APIKeyLookup is the narrow interface AuthAPIKey needs from a secrets
// warm cache — accepts anything that can look a key up. *secrets.Cache
// satisfies it. Defined here as a one-method interface so this middleware
// can be unit-tested without standing up a cache.
type APIKeyLookup interface {
	LookupByValue(value string, now time.Time) (secrets.Secret, bool)
}

// AuthAPIKey validates X-API-Key against the secrets warm cache. Returns
// 401 if the header is missing, unknown, or revoked / expired. On success
// the matching Secret is attached to the request context so downstream
// handlers can read it via SecretFromContext (for tenant scoping etc).
//
// Only secrets with purpose=api_key are accepted — even if a JWT-signing
// key happens to match the header value, we reject. Mistakenly presenting
// the wrong purpose value should look like an unknown key, not an auth
// bypass.
//
// Logging: failed auth attempts are logged at WARN with the truncated
// presented value so ops can spot spray attacks. The full presented value
// is never logged (security hygiene).
func AuthAPIKey(cache APIKeyLookup, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			presented := r.Header.Get("X-API-Key")
			if presented == "" {
				// Browser-navigation fallback: <a href=…?api_key=…> can't
				// add an X-API-Key header, but the operator-only dev
				// surfaces (pub sim quick-links, /v1/ssp/placements
				// debug view) need to be clickable. Accept ?api_key= as
				// a fallback for GETs only — POST/PATCH/DELETE callers
				// always have a Fetch / curl handy and should use the
				// header, which stays out of access logs.
				if r.Method == http.MethodGet {
					presented = r.URL.Query().Get("api_key")
				}
			}
			if presented == "" {
				log.Warn("api key auth: missing X-API-Key header",
					"method", r.Method, "path", r.URL.Path,
					"remote", r.RemoteAddr)
				http.Error(w, "missing X-API-Key", http.StatusUnauthorized)
				return
			}
			sec, ok := cache.LookupByValue(presented, time.Now())
			if !ok {
				log.Warn("api key auth: unknown or revoked key",
					"method", r.Method, "path", r.URL.Path,
					"remote", r.RemoteAddr,
					"key_prefix", truncatedKey(presented))
				http.Error(w, "invalid API key", http.StatusUnauthorized)
				return
			}
			if sec.Purpose != secrets.PurposeAPIKey {
				// Defence in depth: if a JWT signing key value happens
				// to collide with the presented header, don't accept.
				log.Warn("api key auth: matched secret has wrong purpose",
					"key_prefix", truncatedKey(presented),
					"purpose", sec.Purpose,
					"name", sec.Name)
				http.Error(w, "invalid API key", http.StatusUnauthorized)
				return
			}
			ctx := context.WithValue(r.Context(), apiKeyCtxKey{}, &sec)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// truncatedKey returns the first 8 chars of a presented credential, used
// in failed-auth log lines to help ops correlate spray patterns without
// echoing full secret material into logs.
func truncatedKey(v string) string {
	const max = 8
	if len(v) > max {
		return v[:max] + "…"
	}
	return strings.Repeat("*", len(v))
}
