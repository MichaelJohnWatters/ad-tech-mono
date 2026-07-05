package main

import (
	"context"
	"database/sql"
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
	gotPatchID string
	gotPatch   dealPatchInput
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
func (f *fakeDealStore) UpdateDeal(_ context.Context, accountID, id string, in dealPatchInput) error {
	if f.notOwned {
		return sql.ErrNoRows
	}
	f.gotAccount = accountID
	f.gotPatchID = id
	f.gotPatch = in
	return nil
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

	// Allowlist + flight-date depth: valid create parses the new fields.
	store = &fakeDealStore{}
	rec = httptest.NewRecorder()
	body := `{"publisher_id":"p1","name":"PMP","deal_type":"pmp",
		"advertiser_ids":["bbbbbbb1-1111-4111-8111-111111111111"],
		"placement_ids":["ccccccc1-1111-4111-8111-111111111111"],
		"guaranteed_volume":10000,"start_date":"2026-07-01","end_date":"2026-09-30"}`
	dealsHandler(store, nil, quietLog())(rec, dealReq(http.MethodPost, body, pub))
	if rec.Code != http.StatusCreated {
		t.Fatalf("depth create code = %d, want 201; body %s", rec.Code, rec.Body.String())
	}
	if len(store.gotInput.AdvertiserIDs) != 1 || len(store.gotInput.PlacementIDs) != 1 ||
		store.gotInput.GuaranteedVolume != 10000 || store.gotInput.StartDate != "2026-07-01" {
		t.Errorf("depth fields not parsed: %+v", store.gotInput)
	}

	// Bad UUID in allowlist / bad date / end<start / negative volume → 400.
	for _, tc := range []string{
		`{"publisher_id":"p1","name":"X","advertiser_ids":["not-a-uuid"]}`,
		`{"publisher_id":"p1","name":"X","placement_ids":["nope"]}`,
		`{"publisher_id":"p1","name":"X","start_date":"07-01-2026"}`,
		`{"publisher_id":"p1","name":"X","start_date":"2026-09-30","end_date":"2026-07-01"}`,
		`{"publisher_id":"p1","name":"X","guaranteed_volume":-5}`,
	} {
		rec = httptest.NewRecorder()
		dealsHandler(&fakeDealStore{}, nil, quietLog())(rec, dealReq(http.MethodPost, tc, pub))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("invalid deal %s: code = %d, want 400", tc, rec.Code)
		}
	}
}

func dealPatchReq(id, body string, claims *auth.Claims) *http.Request {
	req := httptest.NewRequest(http.MethodPatch, "/v1/api/deals/"+id, strings.NewReader(body))
	if claims != nil {
		req = withClaims(req, claims)
	}
	return req
}

func TestDealByIDHandler(t *testing.T) {
	pub := &auth.Claims{AccountID: "aaaaaaa1-1111-4111-8111-111111111111", Permissions: []string{"deals:update"}}

	// Pause → parsed, scoped.
	store := &fakeDealStore{}
	rec := httptest.NewRecorder()
	dealByIDHandler(store, nil, quietLog())(rec, dealPatchReq("d1", `{"status":"paused"}`, pub))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch code = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if store.gotPatchID != "d1" || store.gotPatch.Status == nil || *store.gotPatch.Status != "paused" {
		t.Errorf("patch not parsed/scoped: id=%q patch=%+v", store.gotPatchID, store.gotPatch)
	}

	// Unknown / foreign deal → 404.
	rec = httptest.NewRecorder()
	dealByIDHandler(&fakeDealStore{notOwned: true}, nil, quietLog())(rec, dealPatchReq("other", `{"status":"paused"}`, pub))
	if rec.Code != http.StatusNotFound {
		t.Errorf("foreign deal code = %d, want 404", rec.Code)
	}

	// Bad status / empty patch → 400.
	for _, body := range []string{`{"status":"archived"}`, `{}`} {
		rec = httptest.NewRecorder()
		dealByIDHandler(&fakeDealStore{}, nil, quietLog())(rec, dealPatchReq("d1", body, pub))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: code = %d, want 400", body, rec.Code)
		}
	}

	// No deals:update → 403.
	rec = httptest.NewRecorder()
	dealByIDHandler(&fakeDealStore{}, nil, quietLog())(rec, dealPatchReq("d1", `{"status":"paused"}`, &auth.Claims{AccountID: "aaaaaaa1-1111-4111-8111-111111111111", Permissions: []string{"deals:read"}}))
	if rec.Code != http.StatusForbidden {
		t.Errorf("no update perm code = %d, want 403", rec.Code)
	}
}
