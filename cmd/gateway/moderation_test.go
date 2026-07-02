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

type fakeModStore struct {
	pending  []moderationItem
	decided  [3]string // id, status, reason
	notFound bool
}

func (f *fakeModStore) ListPending(context.Context) ([]moderationItem, error) {
	return f.pending, nil
}
func (f *fakeModStore) Decide(_ context.Context, id, status, reason, _ string) error {
	if f.notFound {
		return sql.ErrNoRows
	}
	f.decided = [3]string{id, status, reason}
	return nil
}

func modReq(method, body string, claims *auth.Claims) *http.Request {
	req := httptest.NewRequest(method, "/v1/api/moderation", strings.NewReader(body))
	if claims != nil {
		req = withClaims(req, claims)
	}
	return req
}

func TestModerationHandler(t *testing.T) {
	staff := &auth.Claims{UserID: "user-1", AccountType: auth.AccountStaff, Permissions: []string{"moderation:read", "moderation:approve", "moderation:reject"}}

	// List.
	store := &fakeModStore{pending: []moderationItem{{ID: "c1", Name: "banner"}}}
	rec := httptest.NewRecorder()
	moderationHandler(store, quietLog())(rec, modReq(http.MethodGet, "", staff))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "banner") {
		t.Fatalf("list code=%d body=%s", rec.Code, rec.Body.String())
	}

	// Approve.
	store = &fakeModStore{}
	rec = httptest.NewRecorder()
	moderationHandler(store, quietLog())(rec, modReq(http.MethodPost, `{"creative_id":"c1","action":"approve"}`, staff))
	if rec.Code != http.StatusOK || store.decided != [3]string{"c1", "approved", ""} {
		t.Errorf("approve code=%d decided=%v", rec.Code, store.decided)
	}

	// Reject with reason.
	store = &fakeModStore{}
	rec = httptest.NewRecorder()
	moderationHandler(store, quietLog())(rec, modReq(http.MethodPost, `{"creative_id":"c2","action":"reject","reason":"policy"}`, staff))
	if rec.Code != http.StatusOK || store.decided != [3]string{"c2", "rejected", "policy"} {
		t.Errorf("reject code=%d decided=%v", rec.Code, store.decided)
	}

	// Reject without reason → 400.
	rec = httptest.NewRecorder()
	moderationHandler(&fakeModStore{}, quietLog())(rec, modReq(http.MethodPost, `{"creative_id":"c2","action":"reject"}`, staff))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("reject-no-reason code = %d, want 400", rec.Code)
	}

	// Missing moderation perm → 403 (an advertiser can't moderate).
	rec = httptest.NewRecorder()
	moderationHandler(&fakeModStore{}, quietLog())(rec, modReq(http.MethodGet, "", &auth.Claims{Permissions: []string{"campaigns:read"}}))
	if rec.Code != http.StatusForbidden {
		t.Errorf("no perm code = %d, want 403", rec.Code)
	}
}
