package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
)

type fakeDealStore struct {
	list       []dealView
	notOwned   bool
	gotAccount string
	gotInput   dealInput
}

func (f *fakeDealStore) ListDeals(_ context.Context, accountID string) ([]dealView, error) {
	f.gotAccount = accountID
	return f.list, nil
}
func (f *fakeDealStore) CreateDeal(_ context.Context, accountID string, in dealInput) (string, error) {
	if f.notOwned {
		return "", errDealPublisherNotOwned
	}
	f.gotAccount = accountID
	f.gotInput = in
	return "deal-new", nil
}

func dealReq(method, body string, claims *auth.Claims) *http.Request {
	req := httptest.NewRequest(method, "/v1/api/deals", strings.NewReader(body))
	if claims != nil {
		req = withClaims(req, claims)
	}
	return req
}

func TestDealsHandler(t *testing.T) {
	pub := &auth.Claims{AccountID: "aaaaaaa1-1111-4111-8111-111111111111", Permissions: []string{"deals:read", "deals:create"}}

	// List, scoped.
	store := &fakeDealStore{list: []dealView{{ID: "d1", Name: "PMP A"}}}
	rec := httptest.NewRecorder()
	dealsHandler(store, nil, quietLog())(rec, dealReq(http.MethodGet, "", pub))
	if rec.Code != http.StatusOK || store.gotAccount != "aaaaaaa1-1111-4111-8111-111111111111" {
		t.Fatalf("list code=%d account=%q", rec.Code, store.gotAccount)
	}

	// Create valid → 201, scoped + parsed.
	store = &fakeDealStore{}
	rec = httptest.NewRecorder()
	dealsHandler(store, nil, quietLog())(rec, dealReq(http.MethodPost, `{"publisher_id":"p1","name":"Preferred X","deal_type":"preferred","price":3.5}`, pub))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create code = %d, want 201; body %s", rec.Code, rec.Body.String())
	}
	if store.gotAccount != "aaaaaaa1-1111-4111-8111-111111111111" || store.gotInput.Name != "Preferred X" {
		t.Errorf("create not scoped/parsed: %q %+v", store.gotAccount, store.gotInput)
	}

	// Publisher not owned → 403.
	rec = httptest.NewRecorder()
	dealsHandler(&fakeDealStore{notOwned: true}, nil, quietLog())(rec, dealReq(http.MethodPost, `{"publisher_id":"other","name":"X"}`, pub))
	if rec.Code != http.StatusForbidden {
		t.Errorf("not-owned code = %d, want 403", rec.Code)
	}

	// Bad deal_type → 400.
	rec = httptest.NewRecorder()
	dealsHandler(&fakeDealStore{}, nil, quietLog())(rec, dealReq(http.MethodPost, `{"publisher_id":"p1","name":"X","deal_type":"barter"}`, pub))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad deal_type code = %d, want 400", rec.Code)
	}

	// No deals:create → 403.
	rec = httptest.NewRecorder()
	dealsHandler(&fakeDealStore{}, nil, quietLog())(rec, dealReq(http.MethodPost, `{"publisher_id":"p1","name":"X"}`, &auth.Claims{AccountID: "aaaaaaa1-1111-4111-8111-111111111111", Permissions: []string{"deals:read"}}))
	if rec.Code != http.StatusForbidden {
		t.Errorf("no create perm code = %d, want 403", rec.Code)
	}
}
