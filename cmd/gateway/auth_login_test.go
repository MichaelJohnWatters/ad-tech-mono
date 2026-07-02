package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"golang.org/x/crypto/bcrypt"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func postForm(h http.HandlerFunc, email, password string) *httptest.ResponseRecorder {
	form := url.Values{"email": {email}, "password": {password}}
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func TestLoginSubmit(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	lookup := func(_ context.Context, email string) (*teamMember, error) {
		if email != "adv@x.com" {
			return nil, nil // unknown
		}
		return &teamMember{ID: "u1", AccountID: "a1", AccountType: auth.AccountAdvertiser, Role: auth.RoleOwner, PasswordHash: string(hash)}, nil
	}
	h := loginSubmitHandler(lookup, "test-key", quietLog())

	// Valid → 303 + httpOnly session cookie + persona redirect.
	rec := postForm(h, "adv@x.com", "secret")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("valid login code = %d, want 303", rec.Code)
	}
	var gotCookie bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == middleware.SessionCookieName && c.Value != "" && c.HttpOnly {
			gotCookie = true
		}
	}
	if !gotCookie {
		t.Errorf("expected an httpOnly session cookie with a token")
	}
	if loc := rec.Header().Get("Location"); loc != "/dev/portal/advertiser" {
		t.Errorf("redirect = %q, want /dev/portal/advertiser", loc)
	}

	// Wrong password → 401, no cookie.
	rec = postForm(h, "adv@x.com", "wrong")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong password code = %d, want 401", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == middleware.SessionCookieName && c.Value != "" {
			t.Errorf("cookie set on failed login")
		}
	}

	// Unknown user → 401 (same response as wrong password).
	if rec := postForm(h, "nobody@x.com", "secret"); rec.Code != http.StatusUnauthorized {
		t.Errorf("unknown user code = %d, want 401", rec.Code)
	}

	// Missing fields → 400.
	if rec := postForm(h, "", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("missing fields code = %d, want 400", rec.Code)
	}
}
