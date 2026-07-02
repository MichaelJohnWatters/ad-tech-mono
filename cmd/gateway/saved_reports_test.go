package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
)

type fakeSavedReportStore struct {
	list       []savedReportView
	createdAcc string
	createdIn  savedReportInput
	deletedID  string
	deletedAcc string
	notFound   bool
}

func (f *fakeSavedReportStore) ListSavedReports(_ context.Context, accountID string) ([]savedReportView, error) {
	return f.list, nil
}
func (f *fakeSavedReportStore) CreateSavedReport(_ context.Context, accountID string, in savedReportInput) (string, error) {
	f.createdAcc, f.createdIn = accountID, in
	return "rep-1", nil
}
func (f *fakeSavedReportStore) DeleteSavedReport(_ context.Context, accountID, id string) error {
	if f.notFound {
		return sql.ErrNoRows
	}
	f.deletedAcc, f.deletedID = accountID, id
	return nil
}

func srReq(method, target, body string, claims *auth.Claims) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if claims != nil {
		req = withClaims(req, claims)
	}
	return req
}

const srPath = "/v1/api/reports/saved"

func TestSavedReportsHandler(t *testing.T) {
	adv := &auth.Claims{AccountID: "acc-1", AccountType: auth.AccountAdvertiser,
		Permissions: []string{"reports:read", "reports:save"}}

	// List.
	store := &fakeSavedReportStore{list: []savedReportView{{ID: "r1", Name: "Daily spend", QueryConfig: json.RawMessage(`{}`)}}}
	rec := httptest.NewRecorder()
	savedReportsHandler(store, quietLog())(rec, srReq(http.MethodGet, srPath, "", adv))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Daily spend") {
		t.Fatalf("list code=%d body=%s", rec.Code, rec.Body.String())
	}

	// Create scoped to account, schedule + delivery preserved.
	store = &fakeSavedReportStore{}
	rec = httptest.NewRecorder()
	savedReportsHandler(store, quietLog())(rec, srReq(http.MethodPost, srPath,
		`{"name":"Weekly","query_config":{"dims":["campaign"]},"schedule":"0 9 * * 1","delivery":"email"}`, adv))
	if rec.Code != http.StatusCreated || store.createdAcc != "acc-1" {
		t.Fatalf("create code=%d acc=%q (%s)", rec.Code, store.createdAcc, rec.Body.String())
	}
	if store.createdIn.Schedule != "0 9 * * 1" || store.createdIn.Delivery != "email" {
		t.Errorf("create schedule=%q delivery=%q", store.createdIn.Schedule, store.createdIn.Delivery)
	}

	// Create missing name → 400.
	rec = httptest.NewRecorder()
	savedReportsHandler(&fakeSavedReportStore{}, quietLog())(rec, srReq(http.MethodPost, srPath, `{"query_config":{}}`, adv))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("no name code = %d, want 400", rec.Code)
	}

	// Create invalid query_config → 400.
	rec = httptest.NewRecorder()
	savedReportsHandler(&fakeSavedReportStore{}, quietLog())(rec, srReq(http.MethodPost, srPath, `{"name":"x"}`, adv))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("no query_config code = %d, want 400", rec.Code)
	}

	// Create bad delivery → 400.
	rec = httptest.NewRecorder()
	savedReportsHandler(&fakeSavedReportStore{}, quietLog())(rec, srReq(http.MethodPost, srPath, `{"name":"x","query_config":{},"delivery":"sms"}`, adv))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad delivery code = %d, want 400", rec.Code)
	}

	// Delete scoped to account.
	store = &fakeSavedReportStore{}
	rec = httptest.NewRecorder()
	savedReportsHandler(store, quietLog())(rec, srReq(http.MethodDelete, srPath+"?id=r1", "", adv))
	if rec.Code != http.StatusOK || store.deletedID != "r1" || store.deletedAcc != "acc-1" {
		t.Errorf("delete code=%d id=%q acc=%q", rec.Code, store.deletedID, store.deletedAcc)
	}

	// Delete unknown → 404.
	rec = httptest.NewRecorder()
	savedReportsHandler(&fakeSavedReportStore{notFound: true}, quietLog())(rec, srReq(http.MethodDelete, srPath+"?id=zzz", "", adv))
	if rec.Code != http.StatusNotFound {
		t.Errorf("delete-unknown code = %d, want 404", rec.Code)
	}

	// Missing perm → 403.
	rec = httptest.NewRecorder()
	savedReportsHandler(&fakeSavedReportStore{}, quietLog())(rec, srReq(http.MethodGet, srPath, "", &auth.Claims{Permissions: []string{"campaigns:read"}}))
	if rec.Code != http.StatusForbidden {
		t.Errorf("no perm code = %d, want 403", rec.Code)
	}
}
