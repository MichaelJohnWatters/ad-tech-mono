package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
)

type fakeSignupStore struct {
	taken     bool
	createErr error
	created   signupInput
}

func (f *fakeSignupStore) EmailTaken(_ context.Context, _ string) (bool, error) { return f.taken, nil }
func (f *fakeSignupStore) CreateAccountWithOwner(_ context.Context, in signupInput, _ string) (string, string, error) {
	if f.createErr != nil {
		return "", "", f.createErr
	}
	f.created = in
	return "acct-new", "user-new", nil
}

func postSignup(h http.HandlerFunc, vals url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/signup", strings.NewReader(vals.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func TestSignup(t *testing.T) {
	store := &fakeSignupStore{}
	h := signupHandler(store, "key", quietLog())

	// Valid advertiser → 303 + session cookie + advertiser portal.
	rec := postSignup(h, url.Values{"name": {"Acme"}, "email": {"A@Acme.com"}, "password": {"secret1"}, "account_type": {"advertiser"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("valid signup code = %d, want 303", rec.Code)
	}
	if store.created.Email != "a@acme.com" {
		t.Errorf("email should be lowercased, got %q", store.created.Email)
	}
	var cookie bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == middleware.SessionCookieName && c.Value != "" {
			cookie = true
		}
	}
	if !cookie {
		t.Errorf("expected a session cookie")
	}
	if loc := rec.Header().Get("Location"); loc != "/dev/portal/advertiser" {
		t.Errorf("redirect = %q, want advertiser portal", loc)
	}

	// Duplicate email → 409.
	if rec := postSignup(fakeSignupHandlerWith(true), url.Values{"name": {"X"}, "email": {"x@x.com"}, "password": {"secret1"}, "account_type": {"publisher"}}); rec.Code != http.StatusConflict {
		t.Errorf("dup email code = %d, want 409", rec.Code)
	}

	// Bad account type → 400.
	if rec := postSignup(h, url.Values{"name": {"X"}, "email": {"x@x.com"}, "password": {"secret1"}, "account_type": {"admin"}}); rec.Code != http.StatusBadRequest {
		t.Errorf("bad account_type code = %d, want 400", rec.Code)
	}
	// Short password → 400.
	if rec := postSignup(h, url.Values{"name": {"X"}, "email": {"x@x.com"}, "password": {"no"}, "account_type": {"advertiser"}}); rec.Code != http.StatusBadRequest {
		t.Errorf("short password code = %d, want 400", rec.Code)
	}
}

// fakeSignupHandlerWith builds a handler whose store reports email taken.
func fakeSignupHandlerWith(taken bool) http.HandlerFunc {
	return signupHandler(&fakeSignupStore{taken: taken}, "key", quietLog())
}
