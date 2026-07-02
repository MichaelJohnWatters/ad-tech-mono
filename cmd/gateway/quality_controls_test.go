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

type fakeQCStore struct {
	list       []qualityControlView
	upsertedIn qualityControlInput
	upsertAcc  string
	notOwned   bool
	deletedID  string
	deletedAcc string
	notFound   bool
}

func (f *fakeQCStore) ListQualityControls(_ context.Context, accountID string) ([]qualityControlView, error) {
	return f.list, nil
}
func (f *fakeQCStore) UpsertQualityControl(_ context.Context, accountID string, in qualityControlInput) (string, error) {
	if f.notOwned {
		return "", errQCPublisherNotOwned
	}
	f.upsertAcc, f.upsertedIn = accountID, in
	return "qc-1", nil
}
func (f *fakeQCStore) DeleteQualityControl(_ context.Context, accountID, id string) error {
	if f.notFound {
		return sql.ErrNoRows
	}
	f.deletedAcc, f.deletedID = accountID, id
	return nil
}

func qcReq(method, target, body string, claims *auth.Claims) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if claims != nil {
		req = withClaims(req, claims)
	}
	return req
}

const qcPath = "/v1/api/quality-controls"

func TestQualityControlsHandler(t *testing.T) {
	pub := &auth.Claims{AccountID: "acc-1", AccountType: auth.AccountPublisher,
		Permissions: []string{"quality:read", "quality:update"}}

	// List.
	store := &fakeQCStore{list: []qualityControlView{{ID: "q1", Type: "domain_blocklist", Values: []string{"bad.test"}}}}
	rec := httptest.NewRecorder()
	qualityControlsHandler(store, quietLog())(rec, qcReq(http.MethodGet, qcPath, "", pub))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "domain_blocklist") {
		t.Fatalf("list code=%d body=%s", rec.Code, rec.Body.String())
	}

	// Upsert scoped to account, values cleaned (dedup + drop empty).
	store = &fakeQCStore{}
	rec = httptest.NewRecorder()
	qualityControlsHandler(store, quietLog())(rec, qcReq(http.MethodPost, qcPath,
		`{"publisher_id":"pub-1","type":"advertiser_blocklist","values":["a","a",""]}`, pub))
	if rec.Code != http.StatusOK || store.upsertAcc != "acc-1" {
		t.Fatalf("upsert code=%d acc=%q (%s)", rec.Code, store.upsertAcc, rec.Body.String())
	}
	if len(store.upsertedIn.Values) != 1 {
		t.Errorf("values not cleaned: %v", store.upsertedIn.Values)
	}

	// Upsert bad type → 400.
	rec = httptest.NewRecorder()
	qualityControlsHandler(&fakeQCStore{}, quietLog())(rec, qcReq(http.MethodPost, qcPath, `{"publisher_id":"p","type":"nope"}`, pub))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad type code = %d, want 400", rec.Code)
	}

	// Upsert publisher not owned → 403.
	rec = httptest.NewRecorder()
	qualityControlsHandler(&fakeQCStore{notOwned: true}, quietLog())(rec, qcReq(http.MethodPost, qcPath, `{"publisher_id":"other","type":"domain_blocklist"}`, pub))
	if rec.Code != http.StatusForbidden {
		t.Errorf("not-owned code = %d, want 403", rec.Code)
	}

	// Delete scoped to account.
	store = &fakeQCStore{}
	rec = httptest.NewRecorder()
	qualityControlsHandler(store, quietLog())(rec, qcReq(http.MethodDelete, qcPath+"?id=q1", "", pub))
	if rec.Code != http.StatusOK || store.deletedID != "q1" || store.deletedAcc != "acc-1" {
		t.Errorf("delete code=%d id=%q acc=%q", rec.Code, store.deletedID, store.deletedAcc)
	}

	// Delete unknown → 404.
	rec = httptest.NewRecorder()
	qualityControlsHandler(&fakeQCStore{notFound: true}, quietLog())(rec, qcReq(http.MethodDelete, qcPath+"?id=zzz", "", pub))
	if rec.Code != http.StatusNotFound {
		t.Errorf("delete-unknown code = %d, want 404", rec.Code)
	}

	// Missing perm → 403.
	rec = httptest.NewRecorder()
	qualityControlsHandler(&fakeQCStore{}, quietLog())(rec, qcReq(http.MethodGet, qcPath, "", &auth.Claims{Permissions: []string{"campaigns:read"}}))
	if rec.Code != http.StatusForbidden {
		t.Errorf("no perm code = %d, want 403", rec.Code)
	}
}
