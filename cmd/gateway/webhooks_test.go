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
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
)

type fakeWebhookStore struct {
	list       []webhookView
	createdIn  webhookInput
	createdSec string
	createdAcc string
	deletedID  string
	deletedAcc string
	notFound   bool
}

func (f *fakeWebhookStore) ListWebhooks(_ context.Context, accountID string) ([]webhookView, error) {
	return f.list, nil
}
func (f *fakeWebhookStore) CreateWebhook(_ context.Context, accountID string, in webhookInput, secret string) (string, error) {
	f.createdAcc, f.createdIn, f.createdSec = accountID, in, secret
	return "wh-1", nil
}
func (f *fakeWebhookStore) DeleteWebhook(_ context.Context, accountID, id string) error {
	if f.notFound {
		return sql.ErrNoRows
	}
	f.deletedAcc, f.deletedID = accountID, id
	return nil
}

func whReq(method, target, body string, claims *auth.Claims) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if claims != nil {
		req = withClaims(req, claims)
	}
	return req
}

const whPath = "/v1/api/webhooks"

func TestWebhooksHandler(t *testing.T) {
	adv := &auth.Claims{AccountID: "acc-1", AccountType: auth.AccountAdvertiser,
		Permissions: []string{"webhooks:read", "webhooks:create", "webhooks:delete"}}

	// List scoped to caller's account.
	store := &fakeWebhookStore{list: []webhookView{{ID: "w1", URL: "https://x.test", Events: []string{"invoice.generated"}}}}
	rec := httptest.NewRecorder()
	webhooksHandler(store, nil, quietLog())(rec, whReq(http.MethodGet, whPath, "", adv))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "invoice.generated") {
		t.Fatalf("list code=%d body=%s", rec.Code, rec.Body.String())
	}

	// Create → returns secret once, publishes invalidate, scoped to account.
	store = &fakeWebhookStore{}
	bus := &countingBus{}
	rec = httptest.NewRecorder()
	webhooksHandler(store, bus, quietLog())(rec, whReq(http.MethodPost, whPath,
		`{"url":"https://hooks.test/x","events":["budget.depleted","budget.depleted",""]}`, adv))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create code = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	if store.createdAcc != "acc-1" {
		t.Errorf("create account = %q, want acc-1", store.createdAcc)
	}
	if len(store.createdIn.Events) != 1 { // deduped + empties dropped
		t.Errorf("create events = %v, want 1 cleaned entry", store.createdIn.Events)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["secret"] == nil || body["secret"] == "" {
		t.Errorf("create response missing secret")
	}
	if bus.published != 1 || bus.subject != events.SubjectCacheInvalidateWebhookSubs {
		t.Errorf("create invalidate: published=%d subject=%q", bus.published, bus.subject)
	}

	// Create with non-http URL → 400.
	rec = httptest.NewRecorder()
	webhooksHandler(&fakeWebhookStore{}, nil, quietLog())(rec, whReq(http.MethodPost, whPath, `{"url":"ftp://x","events":["a"]}`, adv))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad url code = %d, want 400", rec.Code)
	}

	// Create with no events → 400.
	rec = httptest.NewRecorder()
	webhooksHandler(&fakeWebhookStore{}, nil, quietLog())(rec, whReq(http.MethodPost, whPath, `{"url":"https://x.test","events":[]}`, adv))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("no events code = %d, want 400", rec.Code)
	}

	// Delete scoped to account.
	store = &fakeWebhookStore{}
	rec = httptest.NewRecorder()
	webhooksHandler(store, nil, quietLog())(rec, whReq(http.MethodDelete, whPath+"?id=w1", "", adv))
	if rec.Code != http.StatusOK || store.deletedID != "w1" || store.deletedAcc != "acc-1" {
		t.Errorf("delete code=%d id=%q acc=%q", rec.Code, store.deletedID, store.deletedAcc)
	}

	// Delete unknown → 404.
	rec = httptest.NewRecorder()
	webhooksHandler(&fakeWebhookStore{notFound: true}, nil, quietLog())(rec, whReq(http.MethodDelete, whPath+"?id=zzz", "", adv))
	if rec.Code != http.StatusNotFound {
		t.Errorf("delete-unknown code = %d, want 404", rec.Code)
	}

	// Missing perm → 403.
	rec = httptest.NewRecorder()
	webhooksHandler(&fakeWebhookStore{}, nil, quietLog())(rec, whReq(http.MethodGet, whPath, "", &auth.Claims{Permissions: []string{"campaigns:read"}}))
	if rec.Code != http.StatusForbidden {
		t.Errorf("no perm code = %d, want 403", rec.Code)
	}
}
