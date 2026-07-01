package middleware

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/secrets"
)

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
