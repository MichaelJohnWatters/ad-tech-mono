package main

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
)

type fakePrivacyConsoleStore struct {
	row      privacyOptOutView
	found    bool
	recent   []privacyOptOutView
	lookedUp string
}

func (f *fakePrivacyConsoleStore) Lookup(_ context.Context, id string) (privacyOptOutView, error) {
	f.lookedUp = id
	if !f.found {
		return privacyOptOutView{}, sql.ErrNoRows
	}
	return f.row, nil
}

func (f *fakePrivacyConsoleStore) Recent(context.Context, int) ([]privacyOptOutView, error) {
	return f.recent, nil
}

func TestPrivacyStatusHandler(t *testing.T) {
	staff := &auth.Claims{UserID: "u1", AccountType: auth.AccountStaff, Permissions: []string{"support:read"}}
	adv := &auth.Claims{UserID: "u2", AccountID: "adv-1", AccountType: auth.AccountAdvertiser, Permissions: []string{"campaigns:read"}}

	// Staff lookup of an opted-out identity → 200 with the registry row.
	store := &fakePrivacyConsoleStore{found: true, row: privacyOptOutView{
		UserID: "user-9", Level: 2, Source: "gpc", RequestedAt: time.Now(),
	}}
	rec := httptest.NewRecorder()
	privacyStatusHandler(store, quietLog())(rec, withClaims(httptest.NewRequest(http.MethodGet, "/v1/api/privacy/status?id=user-9", nil), staff))
	if rec.Code != http.StatusOK || store.lookedUp != "user-9" {
		t.Fatalf("staff lookup code=%d lookedUp=%q body=%s", rec.Code, store.lookedUp, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`"opted_out":true`, `"level":2`, `"source":"gpc"`} {
		if !strings.Contains(body, want) {
			t.Errorf("status body missing %s: %s", want, body)
		}
	}

	// Never-opted-out identity → 200 with opted_out:false (not a 404).
	rec = httptest.NewRecorder()
	privacyStatusHandler(&fakePrivacyConsoleStore{}, quietLog())(rec, withClaims(httptest.NewRequest(http.MethodGet, "/v1/api/privacy/status?id=user-clean", nil), staff))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"opted_out":false`) {
		t.Errorf("clean identity code=%d body=%s, want 200 opted_out:false", rec.Code, rec.Body.String())
	}

	// Missing id → 400.
	rec = httptest.NewRecorder()
	privacyStatusHandler(&fakePrivacyConsoleStore{}, quietLog())(rec, withClaims(httptest.NewRequest(http.MethodGet, "/v1/api/privacy/status", nil), staff))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing id code=%d, want 400", rec.Code)
	}

	// Non-staff (no support:read) → 403.
	rec = httptest.NewRecorder()
	privacyStatusHandler(&fakePrivacyConsoleStore{}, quietLog())(rec, withClaims(httptest.NewRequest(http.MethodGet, "/v1/api/privacy/status?id=user-9", nil), adv))
	if rec.Code != http.StatusForbidden {
		t.Errorf("advertiser lookup code=%d, want 403", rec.Code)
	}

	// POST → 405 (status is read-only).
	rec = httptest.NewRecorder()
	privacyStatusHandler(&fakePrivacyConsoleStore{}, quietLog())(rec, withClaims(httptest.NewRequest(http.MethodPost, "/v1/api/privacy/status?id=user-9", nil), staff))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status code=%d, want 405", rec.Code)
	}
}

func TestPrivacyOptOutsHandler(t *testing.T) {
	readOnly := &auth.Claims{UserID: "u1", AccountType: auth.AccountStaff, Permissions: []string{"support:read"}}
	support := &auth.Claims{UserID: "u2", AccountType: auth.AccountStaff, Permissions: []string{"support:read", "support:update"}}
	adv := &auth.Claims{UserID: "u3", AccountID: "adv-1", AccountType: auth.AccountAdvertiser, Permissions: []string{"campaigns:read"}}

	intakeCalled := false
	intake := func(w http.ResponseWriter, r *http.Request) {
		intakeCalled = true
		w.WriteHeader(http.StatusOK)
	}

	// Staff GET → 200 with the recent list.
	store := &fakePrivacyConsoleStore{recent: []privacyOptOutView{
		{UserID: "user-1", Level: 1, Source: "dsar_portal", RequestedAt: time.Now()},
		{UserID: "user-2", Level: 3, Source: "support", RequestedAt: time.Now()},
	}}
	rec := httptest.NewRecorder()
	privacyOptOutsHandler(store, intake, quietLog())(rec, withClaims(httptest.NewRequest(http.MethodGet, "/v1/api/privacy/optouts", nil), readOnly))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "user-2") {
		t.Fatalf("staff GET code=%d body=%s", rec.Code, rec.Body.String())
	}

	// Advertiser GET → 403.
	rec = httptest.NewRecorder()
	privacyOptOutsHandler(store, intake, quietLog())(rec, withClaims(httptest.NewRequest(http.MethodGet, "/v1/api/privacy/optouts", nil), adv))
	if rec.Code != http.StatusForbidden {
		t.Errorf("advertiser GET code=%d, want 403", rec.Code)
	}

	// Staff POST with support:update → delegates to the intake recorder.
	rec = httptest.NewRecorder()
	privacyOptOutsHandler(store, intake, quietLog())(rec, withClaims(
		httptest.NewRequest(http.MethodPost, "/v1/api/privacy/optouts", strings.NewReader(`{"user_id":"user-9","level":2,"source":"support"}`)), support))
	if rec.Code != http.StatusOK || !intakeCalled {
		t.Errorf("support POST code=%d intakeCalled=%v, want 200 true", rec.Code, intakeCalled)
	}

	// Read-only staff POST (no support:update) → 403, intake never runs.
	intakeCalled = false
	rec = httptest.NewRecorder()
	privacyOptOutsHandler(store, intake, quietLog())(rec, withClaims(
		httptest.NewRequest(http.MethodPost, "/v1/api/privacy/optouts", strings.NewReader(`{"user_id":"user-9","level":2}`)), readOnly))
	if rec.Code != http.StatusForbidden || intakeCalled {
		t.Errorf("read-only POST code=%d intakeCalled=%v, want 403 false", rec.Code, intakeCalled)
	}

	// Nil intake (Postgres down at boot) → 503.
	rec = httptest.NewRecorder()
	privacyOptOutsHandler(store, nil, quietLog())(rec, withClaims(
		httptest.NewRequest(http.MethodPost, "/v1/api/privacy/optouts", strings.NewReader(`{"user_id":"user-9","level":2}`)), support))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("nil-intake POST code=%d, want 503", rec.Code)
	}

	// DELETE → 405.
	rec = httptest.NewRecorder()
	privacyOptOutsHandler(store, intake, quietLog())(rec, withClaims(httptest.NewRequest(http.MethodDelete, "/v1/api/privacy/optouts", nil), support))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE code=%d, want 405", rec.Code)
	}
}
