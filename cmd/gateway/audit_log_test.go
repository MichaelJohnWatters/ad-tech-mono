package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
)

type fakeAuditStore struct {
	entries []auditEntryView
	gotQ    auditQuery
}

func (f *fakeAuditStore) ListAudit(_ context.Context, q auditQuery) ([]auditEntryView, error) {
	f.gotQ = q
	return f.entries, nil
}

func auditReq(url string, claims *auth.Claims) *http.Request {
	req := httptest.NewRequest(http.MethodGet, url, nil)
	if claims != nil {
		req = withClaims(req, claims)
	}
	return req
}

func TestAuditLogHandler(t *testing.T) {
	staff := &auth.Claims{AccountID: "staff-1", Permissions: []string{"audit:read"}}

	// Filters parsed, default limit applied.
	store := &fakeAuditStore{entries: []auditEntryView{{ID: "a1", Action: "campaign:update"}}}
	rec := httptest.NewRecorder()
	auditLogHandler(store, quietLog())(rec, auditReq("/v1/api/audit?action=campaign:update&resource_type=line_item", staff))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if store.gotQ.Action != "campaign:update" || store.gotQ.ResourceType != "line_item" || store.gotQ.Limit != 100 {
		t.Errorf("query parsed wrong: %+v", store.gotQ)
	}

	// Limit clamped to 500.
	rec = httptest.NewRecorder()
	auditLogHandler(store, quietLog())(rec, auditReq("/v1/api/audit?limit=9999", staff))
	if store.gotQ.Limit != 500 {
		t.Errorf("limit = %d, want clamped 500", store.gotQ.Limit)
	}

	// Bad limit / bad account_id → 400.
	for _, url := range []string{"/v1/api/audit?limit=zero", "/v1/api/audit?account_id=not-a-uuid"} {
		rec = httptest.NewRecorder()
		auditLogHandler(&fakeAuditStore{}, quietLog())(rec, auditReq(url, staff))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: code = %d, want 400", url, rec.Code)
		}
	}

	// No audit:read → 403 (customer roles never carry it).
	rec = httptest.NewRecorder()
	auditLogHandler(&fakeAuditStore{}, quietLog())(rec, auditReq("/v1/api/audit", &auth.Claims{Permissions: []string{"reports:read"}}))
	if rec.Code != http.StatusForbidden {
		t.Errorf("no perm code = %d, want 403", rec.Code)
	}

	// POST → 405; no claims → 401.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/api/audit", nil)
	auditLogHandler(&fakeAuditStore{}, quietLog())(rec, withClaims(req, staff))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST code = %d, want 405", rec.Code)
	}
	rec = httptest.NewRecorder()
	auditLogHandler(&fakeAuditStore{}, quietLog())(rec, auditReq("/v1/api/audit", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no claims code = %d, want 401", rec.Code)
	}
}
