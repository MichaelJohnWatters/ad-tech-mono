package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
)

type fakeDirectStore struct {
	list      []directLineItemView
	created   directLineItemInput
	gotID     string
	gotPatch  directLineItemPatchInput
	createErr error
}

func (f *fakeDirectStore) ListDirectLineItems(context.Context, string) ([]directLineItemView, error) {
	return f.list, nil
}
func (f *fakeDirectStore) CreateDirectLineItem(_ context.Context, _ string, in directLineItemInput) (string, error) {
	f.created = in
	if f.createErr != nil {
		return "", f.createErr
	}
	return "pli-1", nil
}
func (f *fakeDirectStore) UpdateDirectLineItem(_ context.Context, _ string, id string, in directLineItemPatchInput) error {
	f.gotID, f.gotPatch = id, in
	return nil
}

func TestDirectLineItemsHandler(t *testing.T) {
	pubAcct := "22222222-2222-4222-8222-222222222222"
	pubID := "33333333-3333-4333-8333-333333333333"
	pub := &auth.Claims{UserID: "u1", AccountID: pubAcct, AccountType: auth.AccountPublisher,
		Permissions: []string{"deals:read", "deals:create", "deals:update"}}

	dlReq := func(method, url, body string, claims *auth.Claims) *http.Request {
		req := httptest.NewRequest(method, url, strings.NewReader(body))
		if claims != nil {
			req = withClaims(req, claims)
		}
		return req
	}

	// GET list.
	store := &fakeDirectStore{list: []directLineItemView{{ID: "pli-1", Name: "Nike Sponsorship", PriorityTier: "sponsorship"}}}
	rec := httptest.NewRecorder()
	directLineItemsHandler(store, nil, quietLog())(rec, dlReq(http.MethodGet, "/v1/api/direct-line-items", "", pub))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Nike Sponsorship") {
		t.Fatalf("list code=%d body=%s", rec.Code, rec.Body.String())
	}

	// POST valid → created + publishes publisher-line-items invalidate.
	store = &fakeDirectStore{}
	bus := &countingBus{}
	rec = httptest.NewRecorder()
	body := `{"publisher_id":"` + pubID + `","name":"House ads","priority_tier":"house"}`
	directLineItemsHandler(store, bus, quietLog())(rec, dlReq(http.MethodPost, "/v1/api/direct-line-items", body, pub))
	if rec.Code != http.StatusCreated || store.created.Name != "House ads" {
		t.Fatalf("create code=%d created=%+v", rec.Code, store.created)
	}
	if bus.published != 1 || bus.subject != events.SubjectCacheInvalidatePublisherLineItems {
		t.Errorf("create invalidate: published=%d subject=%q", bus.published, bus.subject)
	}

	// Bad tier / bad pacing / missing name → 400.
	for _, tc := range []string{
		`{"publisher_id":"` + pubID + `","name":"x","priority_tier":"bogus"}`,
		`{"publisher_id":"` + pubID + `","name":"x","priority_tier":"house","pacing_mode":"nope"}`,
		`{"publisher_id":"` + pubID + `","priority_tier":"house"}`,
	} {
		rec = httptest.NewRecorder()
		directLineItemsHandler(&fakeDirectStore{}, nil, quietLog())(rec, dlReq(http.MethodPost, "/v1/api/direct-line-items", tc, pub))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: code=%d, want 400", tc, rec.Code)
		}
	}

	// PATCH valid → parsed + publishes invalidate.
	store = &fakeDirectStore{}
	bus = &countingBus{}
	rec = httptest.NewRecorder()
	directLineItemByIDHandler(store, bus, quietLog())(rec, dlReq(http.MethodPatch, "/v1/api/direct-line-items/pli-9", `{"status":"paused"}`, pub))
	if rec.Code != http.StatusOK || store.gotID != "pli-9" || store.gotPatch.Status == nil || *store.gotPatch.Status != "paused" {
		t.Fatalf("patch code=%d id=%q patch=%+v", rec.Code, store.gotID, store.gotPatch)
	}
	if bus.published != 1 || bus.subject != events.SubjectCacheInvalidatePublisherLineItems {
		t.Errorf("patch invalidate: published=%d subject=%q", bus.published, bus.subject)
	}

	// PATCH bad status → 400.
	rec = httptest.NewRecorder()
	directLineItemByIDHandler(&fakeDirectStore{}, nil, quietLog())(rec, dlReq(http.MethodPatch, "/v1/api/direct-line-items/pli-9", `{"status":"bogus"}`, pub))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad status code=%d, want 400", rec.Code)
	}

	// An advertiser (no deals:* perms) → 403 on read.
	adv := &auth.Claims{AccountID: "44444444-4444-4444-8444-444444444444", AccountType: auth.AccountAdvertiser, Permissions: []string{"campaigns:read"}}
	rec = httptest.NewRecorder()
	directLineItemsHandler(&fakeDirectStore{}, nil, quietLog())(rec, dlReq(http.MethodGet, "/v1/api/direct-line-items", "", adv))
	if rec.Code != http.StatusForbidden {
		t.Errorf("advertiser GET code=%d, want 403", rec.Code)
	}
}
