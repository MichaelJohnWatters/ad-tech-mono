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

// CallerScope resolves the tenant scope of a request. It prefers the API-key
// secret (the direct management path — AuthAPIKey attached it to the context),
// then falls back to the gateway-injected X-Account-ID / X-User-ID headers (the
// JWT path, where the gateway already validated the token). Platform-owned API
// keys (owner in {platform, staff, admin}) are unscoped superusers — this keeps
// the seeded operator key and the pub sim / e2e harness working while making
// account-scoped keys properly isolated.
func CallerScope(r *http.Request) Scope {
	if sec := SecretFromContext(r.Context()); sec != nil {
		switch strings.ToLower(strings.TrimSpace(sec.Owner)) {
		case "platform", "staff", "admin":
			return Scope{Platform: true, Actor: "apikey:" + sec.Name, Resolved: true}
		default:
			return Scope{AccountID: sec.Owner, Actor: "apikey:" + sec.Name, Resolved: true}
		}
	}
	acct := strings.TrimSpace(r.Header.Get(constants.HeaderAccountID))
	if acct != "" {
		user := strings.TrimSpace(r.Header.Get(constants.HeaderUserID))
		actor := "user:" + user
		if user == "" {
			actor = "account:" + acct
		}
		return Scope{AccountID: acct, Actor: actor, Resolved: true}
	}
	return Scope{}
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
