package middleware

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/secrets"
)

func quietMWLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestCallerScope_PlatformKeyIsSuperuser(t *testing.T) {
	r := httptest.NewRequest("PATCH", "/x", nil)
	r = r.WithContext(context.WithValue(r.Context(), apiKeyCtxKey{}, &secrets.Secret{Name: "dev-ops-key", Owner: "platform"}))
	s := CallerScope(r)
	if !s.Platform || !s.Resolved {
		t.Fatalf("platform key: got %+v, want Platform+Resolved", s)
	}
	if !s.CanMutate("any-account-a") || !s.CanMutate("any-account-b") {
		t.Errorf("platform key should be able to mutate any account")
	}
}

func TestCallerScope_AccountKeyIsIsolated(t *testing.T) {
	r := httptest.NewRequest("DELETE", "/x", nil)
	r = r.WithContext(context.WithValue(r.Context(), apiKeyCtxKey{}, &secrets.Secret{Name: "acct-a-key", Owner: "acct-a"}))
	s := CallerScope(r)
	if s.Platform || !s.Resolved || s.AccountID != "acct-a" {
		t.Fatalf("account key: got %+v, want scoped to acct-a", s)
	}
	if !s.CanMutate("acct-a") {
		t.Errorf("account key must mutate its own account")
	}
	if s.CanMutate("acct-b") {
		t.Errorf("account key must NOT mutate another account (the IDOR this closes)")
	}
}

func TestCallerScope_GatewayHeader(t *testing.T) {
	r := httptest.NewRequest("PATCH", "/x", nil)
	r.Header.Set(constants.HeaderAccountID, "acct-c")
	r.Header.Set(constants.HeaderUserID, "user-1")
	s := CallerScope(r)
	if s.Platform || !s.Resolved || s.AccountID != "acct-c" {
		t.Fatalf("gateway header: got %+v, want scoped to acct-c", s)
	}
	if s.Actor != "user:user-1" {
		t.Errorf("actor = %q, want user:user-1", s.Actor)
	}
	if s.CanMutate("acct-d") {
		t.Errorf("header-scoped caller must not mutate another account")
	}
}

// The gateway presents its platform service key on every proxied call AND
// forwards the end-user identity. The forwarded identity must narrow the
// platform key's scope — otherwise every browser session is a superuser at
// the internal services.
func TestCallerScope_PlatformKeyNarrowedByForwardedIdentity(t *testing.T) {
	mk := func(acctType string) Scope {
		r := httptest.NewRequest("PATCH", "/x", nil)
		r = r.WithContext(context.WithValue(r.Context(), apiKeyCtxKey{}, &secrets.Secret{Name: "gw-key", Owner: "platform"}))
		r.Header.Set(constants.HeaderAccountID, "acct-e")
		r.Header.Set(constants.HeaderUserID, "user-9")
		if acctType != "" {
			r.Header.Set(constants.HeaderAccountType, acctType)
		}
		return CallerScope(r)
	}

	// Customer types: scoped to the forwarded account.
	for _, typ := range []string{"advertiser", "publisher", "agency"} {
		s := mk(typ)
		if s.Platform || s.AccountID != "acct-e" || !s.Resolved {
			t.Errorf("%s session: got %+v, want scoped to acct-e", typ, s)
		}
		if s.CanMutate("acct-other") {
			t.Errorf("%s session must not mutate another account through the gateway key", typ)
		}
	}

	// Operator types: stay platform (staff console, dev bypass).
	for _, typ := range []string{"staff", "admin"} {
		s := mk(typ)
		if !s.Platform || !s.Resolved {
			t.Errorf("%s session: got %+v, want platform", typ, s)
		}
	}

	// No forwarded type (legacy X-Account-ID caller): scoped, old semantics.
	if s := mk(""); s.Platform || s.AccountID != "acct-e" {
		t.Errorf("typeless header: got %+v, want scoped to acct-e", s)
	}
}

func TestCallerScope_NoIdentityCannotMutate(t *testing.T) {
	r := httptest.NewRequest("DELETE", "/x", nil)
	s := CallerScope(r)
	if s.Resolved {
		t.Fatalf("no identity should be unresolved, got %+v", s)
	}
	if s.CanMutate("acct-a") {
		t.Errorf("unresolved scope must never be allowed to mutate")
	}
}

// Auth accepts a JWT from the session cookie (browser UI), not just the header.
func TestAuth_CookieSession(t *testing.T) {
	key := "test-signing-key"
	claims := &auth.Claims{UserID: "u1", AccountID: "a1", AccountType: auth.AccountAdvertiser, Role: auth.RoleOwner, Permissions: []string{"campaigns:read"}, ExpiresAt: time.Now().Add(time.Hour)}
	token, err := CreateToken(claims, key)
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	var got *auth.Claims
	h := Auth(key, quietMWLog())(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = ClaimsFromContext(r.Context())
	}))
	req := httptest.NewRequest("GET", "/portal", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got == nil || got.AccountID != "a1" {
		t.Fatalf("cookie session did not authenticate; claims=%+v", got)
	}

	// No header, no cookie → 401.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/portal", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no credentials code = %d, want 401", rec.Code)
	}
}
