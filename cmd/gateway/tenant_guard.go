package main

import (
	"encoding/json"
	"net/http"
	"regexp"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

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
func devTenantGuard(w http.ResponseWriter, r *http.Request, claims *auth.Claims, emptyGET any) bool {
	if claims == nil || claims.AccountID == "" || uuidRe.MatchString(claims.AccountID) {
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
