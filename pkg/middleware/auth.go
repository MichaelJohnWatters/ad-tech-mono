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

// Auth returns middleware that validates JWT tokens and injects claims into context.
// In dev mode (signingKey empty), it creates a default admin claim for all requests.
func Auth(signingKey string, log *slog.Logger) func(http.Handler) http.Handler {
	devMode := signingKey == ""

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

			ctx := context.WithValue(r.Context(), claimsKey{}, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
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

	// Verify signature
	signingInput := parts[0] + "." + parts[1]
	expectedSig := sign(signingInput, signingKey)
	if parts[2] != expectedSig {
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
