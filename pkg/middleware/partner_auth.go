package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/secrets"
)

// PartnerInboundAuth authenticates EXTERNAL inbound OpenRTB traffic (SSP partners /
// Prebid) against a per-partner sandbox key presented as X-API-Key, validated
// against the secrets warm cache (purpose=partner_sandbox; LookupByValue already
// rejects revoked/expired keys). Hot-path-safe — an in-process snapshot scan, no
// per-request DB.
//
// It is the sibling of AuthAPIKey (which is purpose=api_key + always strict): both
// share the one APIKeyLookup + SecretFromContext key-auth path so the platform has a
// single implementation. On success the authenticating *secrets.Secret is injected
// so a downstream handler CAN attribute to the trusted partner account
// (SecretFromContext(ctx).AccountID) instead of a self-declared body field — the
// gate makes that value available; wiring attribution onto it is a separate step.
//
// strict() decides enforcement:
//   - strict:      missing / invalid / wrong-purpose key → 401.
//   - non-strict:  a valid key is still validated + bound, but missing/invalid keys
//     are allowed through (warn — the backwards-compatible default so internal /
//     legacy callers are unaffected during rollout).
//
// Wrap it around the EXTERNAL HTTP registrations only — a trusted internal transport
// (e.g. the exchange's gRPC twin) must be handed the raw handler.
func PartnerInboundAuth(cache APIKeyLookup, strict func() bool, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get("X-API-Key")
			var sec *secrets.Secret
			if key != "" && cache != nil {
				if s, ok := cache.LookupByValue(key, time.Now()); ok && s.Purpose == secrets.PurposePartnerSandbox {
					sec = &s
				}
			}
			if sec == nil {
				if strict() {
					log.Warn("inbound partner auth rejected", "path", r.URL.Path, "has_key", key != "", "remote", r.RemoteAddr)
					w.Header().Set("Content-Type", "application/json")
					http.Error(w, `{"error":"partner authentication required"}`, http.StatusUnauthorized)
					return
				}
				// Warn mode: allow through, flagging a presented-but-invalid key (a
				// real misconfig signal); don't log every anonymous/legacy call.
				if key != "" {
					log.Warn("inbound partner auth: invalid key allowed (non-strict)", "path", r.URL.Path)
				}
				next.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), apiKeyCtxKey{}, sec)))
		})
	}
}
