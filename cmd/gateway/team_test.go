package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
)

type fakeTeamStore struct {
	list       []teamMemberView
	gotAccount string
	gotCreate  [4]string // account, email, name, role
}

func (f *fakeTeamStore) ListTeam(_ context.Context, accountID string) ([]teamMemberView, error) {
	f.gotAccount = accountID
	return f.list, nil
}
func (f *fakeTeamStore) CreateTeamMember(_ context.Context, accountID, email, name, role, _ string) (string, error) {
	f.gotCreate = [4]string{accountID, email, name, role}
	return "tm-new", nil
}

func withClaims(r *http.Request, c *auth.Claims) *http.Request {
	return r.WithContext(middleware.WithClaims(r.Context(), c))
}

func TestTeamHandler_ListRequiresPermAndScopes(t *testing.T) {
	store := &fakeTeamStore{list: []teamMemberView{{ID: "1", Email: "a@x.com", Role: "owner"}}}
	h := teamHandler(store, quietLog())

	// Has team:read → 200, scoped to caller's account.
	claims := &auth.Claims{AccountID: "aaaaaaa4-4444-4444-8444-444444444444", Permissions: []string{"team:read"}}
	rec := httptest.NewRecorder()
	h(rec, withClaims(httptest.NewRequest(http.MethodGet, "/v1/api/team", nil), claims))
	if rec.Code != http.StatusOK {
		t.Fatalf("list code = %d, want 200", rec.Code)
	}
	if store.gotAccount != "aaaaaaa4-4444-4444-8444-444444444444" {
		t.Errorf("list scoped to %q, want acct-7", store.gotAccount)
	}

	// No team:read → 403.
	rec = httptest.NewRecorder()
	h(rec, withClaims(httptest.NewRequest(http.MethodGet, "/v1/api/team", nil), &auth.Claims{AccountID: "aaaaaaa4-4444-4444-8444-444444444444"}))
	if rec.Code != http.StatusForbidden {
		t.Errorf("no perm code = %d, want 403", rec.Code)
	}

	// No claims → 401.
	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/v1/api/team", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no claims code = %d, want 401", rec.Code)
	}
}

func TestTeamHandler_Invite(t *testing.T) {
	store := &fakeTeamStore{}
	h := teamHandler(store, quietLog())
	claims := &auth.Claims{AccountID: "aaaaaaa4-4444-4444-8444-444444444444", Permissions: []string{"team:invite"}}

	body := strings.NewReader(`{"email":"new@x.com","name":"New Person","role":"manager"}`)
	rec := httptest.NewRecorder()
	h(rec, withClaims(httptest.NewRequest(http.MethodPost, "/v1/api/team", body), claims))
	if rec.Code != http.StatusCreated {
		t.Fatalf("invite code = %d, want 201; body %s", rec.Code, rec.Body.String())
	}
	if store.gotCreate != [4]string{"aaaaaaa4-4444-4444-8444-444444444444", "new@x.com", "New Person", "manager"} {
		t.Errorf("create args = %v, want scoped to acct-7", store.gotCreate)
	}
	var resp map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["temp_password"] == "" {
		t.Errorf("invite should return a temp_password")
	}

	// Invalid role → 400.
	rec = httptest.NewRecorder()
	h(rec, withClaims(httptest.NewRequest(http.MethodPost, "/v1/api/team", strings.NewReader(`{"email":"x@x.com","name":"X","role":"superuser"}`)), claims))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid role code = %d, want 400", rec.Code)
	}

	// No team:invite → 403.
	rec = httptest.NewRecorder()
	h(rec, withClaims(httptest.NewRequest(http.MethodPost, "/v1/api/team", strings.NewReader(`{"email":"x@x.com","name":"X"}`)), &auth.Claims{AccountID: "aaaaaaa4-4444-4444-8444-444444444444", Permissions: []string{"team:read"}}))
	if rec.Code != http.StatusForbidden {
		t.Errorf("no invite perm code = %d, want 403", rec.Code)
	}
}
