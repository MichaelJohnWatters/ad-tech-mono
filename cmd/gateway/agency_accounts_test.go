package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
)

type fakeAgencyStore struct {
	list          []agencyManagedView
	agencies      []agencyOption
	listForAgency string
	assignAgency  string
	assignManaged string
	assignErr     error
}

func (f *fakeAgencyStore) ListManagedAccounts(_ context.Context, agencyID string) ([]agencyManagedView, error) {
	f.listForAgency = agencyID
	return f.list, nil
}
func (f *fakeAgencyStore) ListAgencies(context.Context) ([]agencyOption, error) {
	return f.agencies, nil
}
func (f *fakeAgencyStore) AssignManagedAccount(_ context.Context, agencyID, managedID string) error {
	f.assignAgency, f.assignManaged = agencyID, managedID
	return f.assignErr
}
func (f *fakeAgencyStore) UnassignManagedAccount(_ context.Context, _, _ string) error { return nil }

func TestAgencyAccountsHandler(t *testing.T) {
	agencyID := "22222222-2222-4222-8222-222222222222"
	advID := "33333333-3333-4333-8333-333333333333"
	staff := &auth.Claims{UserID: "u1", AccountType: auth.AccountStaff, Permissions: []string{"support:read", "support:update"}}
	agency := &auth.Claims{UserID: "u2", AccountID: agencyID, AccountType: auth.AccountAgency, Permissions: []string{"agency:read"}}
	adv := &auth.Claims{UserID: "u3", AccountID: advID, AccountType: auth.AccountAdvertiser, Permissions: []string{"campaigns:read"}}

	aReq := func(method, url, body string, claims *auth.Claims) *http.Request {
		req := httptest.NewRequest(method, url, strings.NewReader(body))
		return withClaims(req, claims)
	}

	// Agency GET → scoped to its own account.
	store := &fakeAgencyStore{list: []agencyManagedView{{AgencyID: agencyID, ManagedID: advID, ManagedName: "Acme"}}}
	rec := httptest.NewRecorder()
	agencyAccountsHandler(store, quietLog())(rec, aReq(http.MethodGet, "/v1/api/agency-accounts", "", agency))
	if rec.Code != http.StatusOK || store.listForAgency != agencyID || !strings.Contains(rec.Body.String(), "Acme") {
		t.Fatalf("agency GET code=%d forAgency=%q body=%s", rec.Code, store.listForAgency, rec.Body.String())
	}

	// Staff GET → all (forAgency empty unless ?agency_id).
	store = &fakeAgencyStore{}
	rec = httptest.NewRecorder()
	agencyAccountsHandler(store, quietLog())(rec, aReq(http.MethodGet, "/v1/api/agency-accounts", "", staff))
	if rec.Code != http.StatusOK || store.listForAgency != "" {
		t.Fatalf("staff GET code=%d forAgency=%q", rec.Code, store.listForAgency)
	}

	// Advertiser GET → 403 (not staff, no agency:read).
	rec = httptest.NewRecorder()
	agencyAccountsHandler(&fakeAgencyStore{}, quietLog())(rec, aReq(http.MethodGet, "/v1/api/agency-accounts", "", adv))
	if rec.Code != http.StatusForbidden {
		t.Errorf("advertiser GET code=%d, want 403", rec.Code)
	}

	// Staff GET ?list=agencies → the agency roster for the assign picker.
	store = &fakeAgencyStore{agencies: []agencyOption{{ID: agencyID, Name: "Bright Media"}}}
	rec = httptest.NewRecorder()
	agencyAccountsHandler(store, quietLog())(rec, aReq(http.MethodGet, "/v1/api/agency-accounts?list=agencies", "", staff))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Bright Media") {
		t.Fatalf("staff roster GET code=%d body=%s", rec.Code, rec.Body.String())
	}

	// Agency GET ?list=agencies → 403 (roster is staff-only).
	rec = httptest.NewRecorder()
	agencyAccountsHandler(&fakeAgencyStore{}, quietLog())(rec, aReq(http.MethodGet, "/v1/api/agency-accounts?list=agencies", "", agency))
	if rec.Code != http.StatusForbidden {
		t.Errorf("agency roster GET code=%d, want 403", rec.Code)
	}

	// Staff POST assign → 201, store got the ids.
	store = &fakeAgencyStore{}
	rec = httptest.NewRecorder()
	agencyAccountsHandler(store, quietLog())(rec, aReq(http.MethodPost, "/v1/api/agency-accounts",
		`{"agency_account_id":"`+agencyID+`","managed_account_id":"`+advID+`"}`, staff))
	if rec.Code != http.StatusCreated || store.assignAgency != agencyID || store.assignManaged != advID {
		t.Fatalf("staff assign code=%d agency=%q managed=%q", rec.Code, store.assignAgency, store.assignManaged)
	}

	// Agency POST → 403 (staff only).
	rec = httptest.NewRecorder()
	agencyAccountsHandler(&fakeAgencyStore{}, quietLog())(rec, aReq(http.MethodPost, "/v1/api/agency-accounts",
		`{"agency_account_id":"`+agencyID+`","managed_account_id":"`+advID+`"}`, agency))
	if rec.Code != http.StatusForbidden {
		t.Errorf("agency POST code=%d, want 403", rec.Code)
	}

	// Bad UUID → 400.
	rec = httptest.NewRecorder()
	agencyAccountsHandler(&fakeAgencyStore{}, quietLog())(rec, aReq(http.MethodPost, "/v1/api/agency-accounts",
		`{"agency_account_id":"nope","managed_account_id":"`+advID+`"}`, staff))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad-uuid assign code=%d, want 400", rec.Code)
	}

	// Store rejects a non-advertiser → 400.
	store = &fakeAgencyStore{assignErr: errNotAdvertiser}
	rec = httptest.NewRecorder()
	agencyAccountsHandler(store, quietLog())(rec, aReq(http.MethodPost, "/v1/api/agency-accounts",
		`{"agency_account_id":"`+agencyID+`","managed_account_id":"`+advID+`"}`, staff))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("non-advertiser assign code=%d, want 400", rec.Code)
	}
}
