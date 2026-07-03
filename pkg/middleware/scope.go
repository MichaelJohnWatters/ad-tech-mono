package middleware

import (
	"net/http"
	"strings"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
)

// Scope is the resolved tenant + actor identity of an authenticated request,
// used for authorization (may this caller mutate this account's resources?)
// and audit (who did it?). Resolve it with CallerScope.
type Scope struct {
	// AccountID is the account the caller is scoped to. Empty for a platform
	// (unscoped) caller, or when no identity could be resolved.
	AccountID string
	// Platform is true for an operator/superuser credential that may act on
	// any account (the seeded owner='platform' key, or staff/admin).
	Platform bool
	// Actor labels who made the request, for the audit log.
	Actor string
	// Resolved reports whether any identity was found. A request that reaches
	// a mutating handler with Resolved=false has no business mutating.
	Resolved bool
}

// CallerScope resolves the tenant scope of a request.
//
// Resolution order:
//
//  1. Account-scoped API key (AuthAPIKey attached the secret to the context,
//     owner is an account) → scoped to that account. Headers can't widen it.
//  2. Platform-owned API key (owner in {platform, staff, admin}) → the KEY
//     authenticates the channel; if the gateway forwarded an end-user
//     identity (X-Account-ID + X-Account-Type), that identity NARROWS the
//     scope — customer types (advertiser/publisher/agency) are scoped to
//     their account, staff/admin stay platform. Without forwarded headers
//     (the pub sim, e2e harness, curl with the operator key) the key is an
//     unscoped superuser, as before.
//  3. No key → the gateway-injected headers alone (the JWT path, where the
//     gateway already validated the token).
//
// Step 2 closes the proxy hole: the gateway presents its platform service
// key on every proxied call, and without the narrowing an advertiser
// browser session would arrive at internal services as a superuser.
func CallerScope(r *http.Request) Scope {
	if sec := SecretFromContext(r.Context()); sec != nil {
		switch strings.ToLower(strings.TrimSpace(sec.Owner)) {
		case "platform", "staff", "admin":
			if hs, ok := headerScope(r); ok {
				return hs
			}
			return Scope{Platform: true, Actor: "apikey:" + sec.Name, Resolved: true}
		default:
			return Scope{AccountID: sec.Owner, Actor: "apikey:" + sec.Name, Resolved: true}
		}
	}
	if hs, ok := headerScope(r); ok {
		return hs
	}
	return Scope{}
}

// headerScope resolves the gateway-forwarded end-user identity. Customer
// account types are scoped to their account; staff/admin resolve platform so
// operator consoles and the dev bypass keep full access. A missing/unknown
// type header keeps the old semantics (scoped) — pre-HeaderAccountType
// callers that set X-Account-ID directly meant "act as this account".
func headerScope(r *http.Request) (Scope, bool) {
	acct := strings.TrimSpace(r.Header.Get(constants.HeaderAccountID))
	if acct == "" {
		return Scope{}, false
	}
	user := strings.TrimSpace(r.Header.Get(constants.HeaderUserID))
	actor := "user:" + user
	if user == "" {
		actor = "account:" + acct
	}
	switch strings.ToLower(strings.TrimSpace(r.Header.Get(constants.HeaderAccountType))) {
	case "staff", "admin":
		return Scope{Platform: true, Actor: actor, Resolved: true}, true
	default:
		return Scope{AccountID: acct, Actor: actor, Resolved: true}, true
	}
}

// CanMutate reports whether this scope may mutate a resource owned by
// targetAccountID. Platform callers may mutate anything; an account-scoped
// caller may only mutate its own account's resources (the tenant-isolation
// check that closes cross-tenant mutation). An unresolved scope may not mutate.
func (s Scope) CanMutate(targetAccountID string) bool {
	if !s.Resolved {
		return false
	}
	if s.Platform {
		return true
	}
	return s.AccountID != "" && s.AccountID == targetAccountID
}
