package middleware

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
)

type claimsKey struct{}

// SessionCookieName is the httpOnly cookie the browser UI stores its JWT in,
// so dashboard page loads authenticate without an Authorization header. API
// clients keep using the header; the middleware accepts either.
const SessionCookieName = "adtech_session"

// RequestIsSecure reports whether the ORIGINAL client connection was HTTPS —
// used to set the Secure flag on session cookies. It's true for a direct TLS
// connection (r.TLS) OR when a TLS-terminating proxy (our Traefik ingress)
// forwarded the request with X-Forwarded-Proto: https. The proxy terminates TLS
// and dials the app over plain HTTP, so r.TLS alone is nil in prod and the
// cookie would wrongly ship without Secure — this closes that gap.
//
// Trusting the client-settable X-Forwarded-Proto is safe HERE (unlike for rate
// limiting): forcing Secure=true only makes the cookie MORE restrictive — a
// client that lies about https then can't receive its own cookie over http, so
// it only harms the liar. Local/test traffic over plain HTTP sets no such header
// and r.TLS is nil, so the cookie stays non-Secure and works without certs.
func RequestIsSecure(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// tokenFromRequest pulls a bearer token from the Authorization header, falling
// back to the session cookie (browser UI). Returns "" if neither is present or
// the header is malformed.
func tokenFromRequest(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if t := strings.TrimPrefix(h, "Bearer "); t != h {
			return t
		}
		return "" // header present but not "Bearer <token>"
	}
	if c, err := r.Cookie(SessionCookieName); err == nil {
		return c.Value
	}
	return ""
}

// ClaimsFromContext retrieves auth claims from the request context.
func ClaimsFromContext(ctx context.Context) *auth.Claims {
	if c, ok := ctx.Value(claimsKey{}).(*auth.Claims); ok {
		return c
	}
	return nil
}

// WithClaims returns a context carrying the given claims. Exposed so handlers
// and tests can attach an identity without routing through the Auth middleware.
func WithClaims(ctx context.Context, c *auth.Claims) context.Context {
	return context.WithValue(ctx, claimsKey{}, c)
}

// AuthOption configures optional Auth middleware behaviour.
type AuthOption func(*authConfig)

type authConfig struct {
	rev        RevocationChecker
	homeRegion string
}

// WithRevocation makes the Auth middleware reject tokens whose session has been
// revoked (logout-everywhere / stolen-token response). A nil checker is ignored.
func WithRevocation(rev RevocationChecker) AuthOption {
	return func(c *authConfig) { c.rev = rev }
}

// WithRegionGate enforces data residency: a request that MUTATES data for an
// account whose ResidencyRegion differs from this deployment's home region is
// rejected 403 (the account's data lives elsewhere; this region must not store
// it). Reads are allowed anywhere, and platform staff/admin are exempt (they
// operate cross-region). Empty homeRegion (or empty account region) disables the
// gate — single-region deployments are unaffected.
func WithRegionGate(homeRegion string) AuthOption {
	return func(c *authConfig) { c.homeRegion = homeRegion }
}

// regionAllowed reports whether a request may proceed under the residency gate.
func regionAllowed(homeRegion string, claims *auth.Claims, method string) bool {
	if homeRegion == "" || claims == nil {
		return true // gate disabled / no identity (auth handles the latter)
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true // reads are not residency-gated
	}
	switch claims.AccountType {
	case auth.AccountStaff, auth.AccountAdmin:
		return true // platform operators are not region-bound
	}
	// Empty = unpinned (legacy/default) → treated as the home region.
	return claims.ResidencyRegion == "" || claims.ResidencyRegion == homeRegion
}

// Auth returns middleware that validates JWT tokens and injects claims into context.
// In dev mode (signingKey empty), it creates a default admin claim for all requests.
func Auth(signingKey string, log *slog.Logger, opts ...AuthOption) func(http.Handler) http.Handler {
	devMode := signingKey == ""
	var ac authConfig
	for _, o := range opts {
		o(&ac)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if devMode {
				// Dev mode: inject default admin claims
				claims := &auth.Claims{
					UserID:      "dev-user",
					AccountID:   "dev-account",
					AccountType: auth.AccountAdmin,
					Role:        auth.RoleOwner,
					Permissions: []string{"*"},
					ExpiresAt:   time.Now().Add(24 * time.Hour),
				}
				ctx := context.WithValue(r.Context(), claimsKey{}, claims)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			// Token from Authorization header or the session cookie (browser).
			token := tokenFromRequest(r)
			if token == "" {
				http.Error(w, `{"error":"missing or invalid credentials"}`, http.StatusUnauthorized)
				return
			}

			claims, err := validateToken(token, signingKey)
			if err != nil {
				log.Debug("auth failed", "error", err)
				http.Error(w, `{"error":"invalid token"}`, http.StatusUnauthorized)
				return
			}

			if claims.ExpiresAt.Before(time.Now()) {
				http.Error(w, `{"error":"token expired"}`, http.StatusUnauthorized)
				return
			}

			// Session revocation (logout-everywhere / stolen-token). Fail-open on a
			// store error — revocation is defence-in-depth over the token's own
			// expiry, so a Redis outage must not lock everyone out.
			if ac.rev != nil {
				if revoked, err := ac.rev.IsRevoked(r.Context(), claims.UserID, claims.IssuedAt); err != nil {
					log.Warn("revocation check failed (allowing token)", "error", err)
				} else if revoked {
					http.Error(w, `{"error":"session revoked"}`, http.StatusUnauthorized)
					return
				}
			}

			if !regionAllowed(ac.homeRegion, claims, r.Method) {
				log.Warn("data residency gate: out-of-region mutation rejected",
					"account", claims.AccountID, "account_region", claims.ResidencyRegion, "home_region", ac.homeRegion, "method", r.Method, "path", r.URL.Path)
				http.Error(w, `{"error":"data residency: this account's data is not served by this region"}`, http.StatusForbidden)
				return
			}

			ctx := context.WithValue(r.Context(), claimsKey{}, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ParseSession extracts and validates the caller's JWT (Authorization header or
// session cookie), returning (claims, true) only if the signature is valid and
// the token isn't expired. Reusable by browser page handlers that want to gate
// on a session and redirect to /login on failure (rather than the JSON 401 the
// Auth middleware returns). signingKey must be non-empty — dev bypass is the
// caller's concern.
func ParseSession(r *http.Request, signingKey string, rev ...RevocationChecker) (*auth.Claims, bool) {
	token := tokenFromRequest(r)
	if token == "" {
		return nil, false
	}
	claims, err := validateToken(token, signingKey)
	if err != nil {
		return nil, false
	}
	if claims.ExpiresAt.Before(time.Now()) {
		return nil, false
	}
	// Optional revocation check (browser page gating): a revoked session is
	// treated as no session so the caller redirects to /login. Fail-open on a
	// store error, same as the Auth middleware.
	if len(rev) > 0 && rev[0] != nil {
		if revoked, err := rev[0].IsRevoked(r.Context(), claims.UserID, claims.IssuedAt); err == nil && revoked {
			return nil, false
		}
	}
	return claims, true
}

// RequirePermission returns middleware that checks the user has a specific permission.
func RequirePermission(permission string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims := ClaimsFromContext(r.Context())
			if claims == nil {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			if !auth.HasPermission(claims, permission) && !auth.HasPermission(claims, "*") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequirePermissionByMethod returns middleware that checks a different
// permission per HTTP method — e.g. GET needs campaigns:read while POST
// needs campaigns:create. Methods absent from the map are rejected with
// 405 so a new verb can't slip through ungated.
func RequirePermissionByMethod(perms map[string]string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			perm, ok := perms[r.Method]
			if !ok {
				http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
				return
			}
			claims := ClaimsFromContext(r.Context())
			if claims == nil {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			if !auth.HasPermission(claims, perm) && !auth.HasPermission(claims, "*") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// CreateToken creates a signed JWT token from claims.
// Uses HS256 (HMAC-SHA256) - simple and sufficient for internal use.
func CreateToken(claims *auth.Claims, signingKey string) (string, error) {
	header := base64url([]byte(`{"alg":"HS256","typ":"JWT"}`))

	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	payloadEnc := base64url(payload)

	signingInput := header + "." + payloadEnc
	sig := sign(signingInput, signingKey)

	return signingInput + "." + sig, nil
}

func validateToken(token, signingKey string) (*auth.Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errInvalidToken
	}

	// Reject any token whose header doesn't declare HS256. Defence-in-depth
	// against algorithm-confusion: the HMAC check below already only accepts
	// HS256 (we never read alg to pick the verifier), but failing fast on a
	// forged "alg":"none"/"RS256" header is explicit and cheap.
	if hdr, err := base64.RawURLEncoding.DecodeString(parts[0]); err != nil {
		return nil, errInvalidToken
	} else {
		var h struct {
			Alg string `json:"alg"`
		}
		if json.Unmarshal(hdr, &h) != nil || h.Alg != "HS256" {
			return nil, errInvalidToken
		}
	}

	// Verify signature with a constant-time compare so the check can't be
	// timing-probed byte-by-byte.
	signingInput := parts[0] + "." + parts[1]
	expectedSig := sign(signingInput, signingKey)
	if !hmac.Equal([]byte(parts[2]), []byte(expectedSig)) {
		return nil, errInvalidSignature
	}

	// Decode payload
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errInvalidToken
	}

	var claims auth.Claims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, errInvalidToken
	}

	return &claims, nil
}

func sign(input, key string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(input))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func base64url(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

type authError string

func (e authError) Error() string { return string(e) }

const (
	errInvalidToken     = authError("invalid token")
	errInvalidSignature = authError("invalid signature")
)
