package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
)

func TestFilterNav(t *testing.T) {
	items := []NavItem{
		{Label: "Dashboard"},                           // always
		{Label: "Campaigns", Perm: "campaigns:read"},   // advertiser
		{Label: "Moderation", Perm: "moderation:read"}, // staff
	}
	adv := &auth.Claims{Permissions: []string{"campaigns:read", "reports:read"}}
	got := filterNav(items, adv)
	if len(got) != 2 || got[1].Label != "Campaigns" {
		t.Errorf("advertiser nav = %v, want Dashboard+Campaigns", labels(got))
	}
	admin := &auth.Claims{Permissions: []string{"*"}}
	if len(filterNav(items, admin)) != 3 {
		t.Errorf("admin should see all nav items")
	}
	if got := filterNav(items, nil); len(got) != 1 || got[0].Label != "Dashboard" {
		t.Errorf("nil claims should see only ungated items, got %v", labels(got))
	}
}

func labels(items []NavItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Label
	}
	return out
}

func TestRequireLoginPage(t *testing.T) {
	reached := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }

	// Dev (empty signing key) → passes through.
	rec := httptest.NewRecorder()
	requireLoginPage("", reached)(rec, httptest.NewRequest("GET", "/dev/portal/advertiser", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("dev bypass code = %d, want 200", rec.Code)
	}

	// Auth on, no session → redirect to /login.
	rec = httptest.NewRecorder()
	requireLoginPage("key", reached)(rec, httptest.NewRequest("GET", "/dev/portal/advertiser", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Errorf("no session: code=%d loc=%q, want 303 /login", rec.Code, rec.Header().Get("Location"))
	}

	// Auth on, valid session cookie → passes through.
	token, _ := middleware.CreateToken(&auth.Claims{AccountID: "a1", ExpiresAt: time.Now().Add(time.Hour)}, "key")
	req := httptest.NewRequest("GET", "/dev/portal/advertiser", nil)
	req.AddCookie(&http.Cookie{Name: middleware.SessionCookieName, Value: token})
	rec = httptest.NewRecorder()
	requireLoginPage("key", reached)(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("valid session code = %d, want 200", rec.Code)
	}
}
