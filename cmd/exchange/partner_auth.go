package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/secrets"
)

// partnerKeyLookup is the slice of the secrets warm cache the inbound gate needs:
// resolve a presented key to its secret (active/rotating only) with zero per-request
// DB I/O. Satisfied by *secrets.Cache.
type partnerKeyLookup interface {
	LookupByValue(value string, now time.Time) (secrets.Secret, bool)
}

type partnerAccountCtxKey struct{}

// partnerAccountFromContext returns the authenticated inbound partner's account id
// (from the validated sandbox key), or "" when the request was not partner-authed.
func partnerAccountFromContext(ctx context.Context) string {
	v, _ := ctx.Value(partnerAccountCtxKey{}).(string)
	return v
}

// partnerAuthGate authenticates EXTERNAL inbound OpenRTB traffic (SSP partners /
// Prebid) against a per-partner sandbox key presented as X-API-Key (#112). It binds
// the request to the TRUSTED partner account carried by the key — never a
// self-declared field in the body — so downstream attribution can't be spoofed.
//
// strict() decides enforcement:
//   - strict:      a missing/invalid/revoked key → 401.
//   - non-strict:  the key is still validated and (when valid) the trusted account
//     is bound + logged, but missing/invalid keys are allowed through (warn mode;
//     the backwards-compatible default so single-tenant/internal callers are
//     unaffected).
//
// Only the HTTP registrations are wrapped — the internal gRPC twin our own SSP
// uses is deliberately NOT gated (trusted transport, no partner key). Validation is
// against the in-process secrets warm cache (hot-path-safe: no per-request DB hit).
func partnerAuthGate(next http.HandlerFunc, cache partnerKeyLookup, strict func() bool, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-API-Key")
		var acct string
		if key != "" && cache != nil {
			if sec, ok := cache.LookupByValue(key, time.Now()); ok && sec.Purpose == secrets.PurposePartnerSandbox {
				acct = sec.AccountID
			}
		}
		if acct == "" {
			if strict() {
				log.Warn("inbound partner auth rejected", "path", r.URL.Path, "has_key", key != "", "remote", r.RemoteAddr)
				w.Header().Set("Content-Type", "application/json")
				http.Error(w, `{"error":"partner authentication required"}`, http.StatusUnauthorized)
				return
			}
			// Warn mode: allow through, but flag a presented-but-invalid key (a real
			// misconfig signal) — don't log every anonymous/legacy call.
			if key != "" {
				log.Warn("inbound partner auth: invalid key allowed (non-strict)", "path", r.URL.Path)
			}
			next(w, r)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), partnerAccountCtxKey{}, acct)))
	}
}
