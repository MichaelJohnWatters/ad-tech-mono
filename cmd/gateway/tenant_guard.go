package main

import (
	"encoding/json"
	"net/http"
	"regexp"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// effectiveAccount resolves which account a gateway-LOCAL advertiser-portal
// handler should scope to. This is the single source of truth for honoring
// staff "viewing as" / agency act-as impersonation in handlers that do their
// OWN store query (reverse-proxied routes are handled by middleware.ReverseProxy,
// which resolves act-as there — do NOT use this for those).
//
// Rules (identical to middleware.ReverseProxy / effectiveReportAccount):
//   - An act-as target is present (staff impersonating, or an agency on a
//     managed advertiser) AND the caller may access it (auth.CanAccessAccount —
//     staff/admin → any; agency → managed set) → scope to the IMPERSONATED
//     account, regardless of the caller's own account type (a staff session
//     carries AccountType=staff and a non-UUID AccountID).
//   - An act-as target the caller CANNOT access (a bad target, or an agency
//     impersonating an unmanaged account) → (", false): the handler must deny.
//     This can never WIDEN scope.
//   - No act-as → the session's own account (claims.AccountID). A plain
//     advertiser still sees only its own; a staff session with no act-as keeps
//     its non-UUID account (devTenantGuard then renders the empty/dev view).
//
// Returns ("", false) ONLY when a present act-as target is not permitted — that
// is the single failure case the caller surfaces as 403. A nil claims caller
// must be rejected before this is called.
func effectiveAccount(r *http.Request, claims *auth.Claims) (string, bool) {
	if target := middleware.ActAsTarget(r); target != "" {
		_, id := middleware.ParseActAsTarget(target)
		if id == "" || !auth.CanAccessAccount(claims, id) {
			return "", false
		}
		return id, true
	}
	return claims.AccountID, true
}

// canAs is the permission gate for gateway-LOCAL advertiser/publisher-portal
// handlers, impersonation-aware. Without it a staff "viewing as" session fails
// the handler's `can(claims, "creatives:read")` check — a staff token does NOT
// carry customer feature permissions (creatives:read, webhooks:read, …), only
// platform ones — so every impersonated LOCAL portal section 403s even though
// the proxied sections work (the proxy forwards X-Account-Type=advertiser and
// the downstream service RBACs on THAT). This is the permission half of the same
// impersonation gap effectiveAccount fixes for the tenant half.
//
// Rule:
//   - Validly impersonating (act-as target the caller CanAccessAccount) → the
//     permission is evaluated against the IMPERSONATED account type's owner
//     default permission set (advertiser:owner / publisher:owner), i.e. the
//     operator sees the whole target portal, exactly as the proxied routes let
//     them. CanAccessAccount is the authority (staff/admin → any; agency →
//     managed advertisers only), so this can never widen beyond what act-as
//     already permits.
//   - Not impersonating → the caller's own permissions (plain `can`). A plain
//     advertiser/publisher still only gets what its own role grants.
func canAs(r *http.Request, claims *auth.Claims, perm string) bool {
	if target := middleware.ActAsTarget(r); target != "" {
		tType, id := middleware.ParseActAsTarget(target)
		if id != "" && auth.CanAccessAccount(claims, id) {
			if auth.HasPermission(claims, "*") {
				return true
			}
			for _, p := range auth.RolePermissions(tType, auth.RoleOwner) {
				if p == perm {
					return true
				}
			}
			// Fall through to the caller's own perms below (e.g. an agency that
			// already holds the permission directly).
		}
	}
	return can(claims, perm)
}

// devTenantGuard handles the dev-bypass identity for tenant-scoped handlers.
//
// The bypass session's AccountID ("dev-account") isn't a real accounts row —
// every store query that casts it (`$1::uuid`) would 500 at Postgres. When
// the account isn't a UUID: GET is answered with the handler's empty view
// (so dev portals render empty states, not errors) and writes are refused
// with a clear 400. Returns true when it wrote the response — the handler
// must return immediately.
//
// Real sessions (signup/login) always carry UUID accounts and pass through.
// An empty AccountID also passes through — that's not the bypass shape, and
// the permission checks downstream reject it.
//
// IMPORTANT: pass the EFFECTIVE account (from effectiveAccount), not
// claims.AccountID — a staff session impersonating a real advertiser carries a
// non-UUID claims.AccountID but a UUID effective account, and must NOT be
// short-circuited to the empty/dev view.
func devTenantGuard(w http.ResponseWriter, r *http.Request, accountID string, emptyGET any) bool {
	if accountID == "" || uuidRe.MatchString(accountID) {
		return false
	}
	switch r.Method {
	case http.MethodGet:
		_ = json.NewEncoder(w).Encode(emptyGET)
	case http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete:
		http.Error(w, `{"error":"this session has no tenant account — log in as a customer user"}`, http.StatusBadRequest)
	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
	return true
}
