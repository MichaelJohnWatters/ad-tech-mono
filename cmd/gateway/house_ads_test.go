package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/houseads"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

type fakeHouseAdStore struct {
	list      []houseads.HouseAd
	created   houseads.Input
	updatedID string
	updated   houseads.Input
	deletedID string
	createErr error
	updateErr error
	deleteErr error
}

func (f *fakeHouseAdStore) List(context.Context) ([]houseads.HouseAd, error) { return f.list, nil }
func (f *fakeHouseAdStore) Create(_ context.Context, in houseads.Input) (string, error) {
	f.created = in
	if f.createErr != nil {
		return "", f.createErr
	}
	return "44444444-4444-4444-8444-444444444444", nil
}
func (f *fakeHouseAdStore) Update(_ context.Context, id string, in houseads.Input) error {
	f.updatedID, f.updated = id, in
	return f.updateErr
}
func (f *fakeHouseAdStore) Delete(_ context.Context, id string) error {
	f.deletedID = id
	return f.deleteErr
}

func haReq(method, target, body string, claims *auth.Claims) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if claims != nil {
		req = withClaims(req, claims)
	}
	return req
}

func TestHouseAdsHandler(t *testing.T) {
	staff := &auth.Claims{UserID: "u1", AccountType: auth.AccountStaff, Permissions: []string{"support:read", "support:update"}}
	adID := "44444444-4444-4444-8444-444444444444"

	// GET list (support:read).
	store := &fakeHouseAdStore{list: []houseads.HouseAd{{ID: adID, Format: "video", Name: "House Video", Enabled: true}}}
	rec := httptest.NewRecorder()
	houseAdsHandler(store, nil, quietLog())(rec, haReq(http.MethodGet, routes.APIHouseAds, "", staff))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "House Video") {
		t.Fatalf("list code=%d body=%s", rec.Code, rec.Body.String())
	}

	// POST create (support:update) → parsed + publishes house-ads invalidate.
	store = &fakeHouseAdStore{}
	bus := &countingBus{}
	rec = httptest.NewRecorder()
	houseAdsHandler(store, bus, quietLog())(rec, haReq(http.MethodPost, routes.APIHouseAds,
		`{"format":"video","name":"Promo","markup":"<VAST/>","enabled":true,"weight":2}`, staff))
	if rec.Code != http.StatusCreated || store.created.Name != "Promo" || store.created.Weight != 2 {
		t.Fatalf("create code=%d created=%+v", rec.Code, store.created)
	}
	if bus.published != 1 || bus.subject != events.SubjectCacheInvalidateHouseAds {
		t.Errorf("create invalidate: published=%d subject=%q", bus.published, bus.subject)
	}

	// Invalid format → 400 (no store call).
	rec = httptest.NewRecorder()
	houseAdsHandler(&fakeHouseAdStore{}, nil, quietLog())(rec, haReq(http.MethodPost, routes.APIHouseAds,
		`{"format":"carousel","name":"x","markup":"y"}`, staff))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad format: code=%d, want 400", rec.Code)
	}

	// Missing markup / missing name → 400.
	for _, body := range []string{`{"format":"video","name":"x"}`, `{"format":"video","markup":"y"}`} {
		rec = httptest.NewRecorder()
		houseAdsHandler(&fakeHouseAdStore{}, nil, quietLog())(rec, haReq(http.MethodPost, routes.APIHouseAds, body, staff))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: code=%d, want 400", body, rec.Code)
		}
	}

	// Non-staff (advertiser) → 403 on both list (support:read) and create
	// (support:update): house ads are staff-gated, platform-global.
	adv := &auth.Claims{AccountType: auth.AccountAdvertiser, Permissions: []string{"reports:read"}}
	rec = httptest.NewRecorder()
	houseAdsHandler(&fakeHouseAdStore{}, nil, quietLog())(rec, haReq(http.MethodGet, routes.APIHouseAds, "", adv))
	if rec.Code != http.StatusForbidden {
		t.Errorf("advertiser GET code=%d, want 403", rec.Code)
	}
	rec = httptest.NewRecorder()
	houseAdsHandler(&fakeHouseAdStore{}, nil, quietLog())(rec, haReq(http.MethodPost, routes.APIHouseAds,
		`{"format":"video","name":"x","markup":"y"}`, adv))
	if rec.Code != http.StatusForbidden {
		t.Errorf("advertiser POST code=%d, want 403", rec.Code)
	}

	// A read-only staff member (support:read but NOT support:update) cannot
	// mutate → 403.
	readOnly := &auth.Claims{AccountType: auth.AccountStaff, Permissions: []string{"support:read"}}
	rec = httptest.NewRecorder()
	houseAdsHandler(&fakeHouseAdStore{}, nil, quietLog())(rec, haReq(http.MethodPost, routes.APIHouseAds,
		`{"format":"video","name":"x","markup":"y"}`, readOnly))
	if rec.Code != http.StatusForbidden {
		t.Errorf("read-only staff POST code=%d, want 403", rec.Code)
	}
}

func TestHouseAdByIDHandler(t *testing.T) {
	staff := &auth.Claims{UserID: "u1", AccountType: auth.AccountStaff, Permissions: []string{"support:read", "support:update"}}
	adID := "44444444-4444-4444-8444-444444444444"

	// PUT update (support:update) → parsed + publishes invalidate.
	store := &fakeHouseAdStore{}
	bus := &countingBus{}
	rec := httptest.NewRecorder()
	houseAdByIDHandler(store, bus, quietLog())(rec, haReq(http.MethodPut, routes.APIHouseAds+"/"+adID,
		`{"format":"native","name":"Edited","markup":"<div/>","enabled":false,"weight":3}`, staff))
	if rec.Code != http.StatusOK || store.updatedID != adID || store.updated.Name != "Edited" {
		t.Fatalf("put code=%d id=%q updated=%+v", rec.Code, store.updatedID, store.updated)
	}
	if bus.published != 1 || bus.subject != events.SubjectCacheInvalidateHouseAds {
		t.Errorf("put invalidate: published=%d subject=%q", bus.published, bus.subject)
	}

	// DELETE (support:update) → publishes invalidate.
	store = &fakeHouseAdStore{}
	bus = &countingBus{}
	rec = httptest.NewRecorder()
	houseAdByIDHandler(store, bus, quietLog())(rec, haReq(http.MethodDelete, routes.APIHouseAds+"/"+adID, "", staff))
	if rec.Code != http.StatusOK || store.deletedID != adID {
		t.Fatalf("delete code=%d deletedID=%q", rec.Code, store.deletedID)
	}
	if bus.published != 1 || bus.subject != events.SubjectCacheInvalidateHouseAds {
		t.Errorf("delete invalidate: published=%d subject=%q", bus.published, bus.subject)
	}

	// Non-UUID id → 400.
	rec = httptest.NewRecorder()
	houseAdByIDHandler(&fakeHouseAdStore{}, nil, quietLog())(rec, haReq(http.MethodPut, routes.APIHouseAds+"/not-a-uuid",
		`{"format":"video","name":"x","markup":"y"}`, staff))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad id code=%d, want 400", rec.Code)
	}

	// Advertiser (no support:update) → 403.
	adv := &auth.Claims{AccountType: auth.AccountAdvertiser, Permissions: []string{"reports:read"}}
	rec = httptest.NewRecorder()
	houseAdByIDHandler(&fakeHouseAdStore{}, nil, quietLog())(rec, haReq(http.MethodDelete, routes.APIHouseAds+"/"+adID, "", adv))
	if rec.Code != http.StatusForbidden {
		t.Errorf("advertiser DELETE code=%d, want 403", rec.Code)
	}
}
