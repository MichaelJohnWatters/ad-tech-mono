package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
)

type fakeTopupStore struct {
	history    topupBalanceResponse
	result     topupResult
	err        error
	gotAccount string
	gotUser    string
	gotInput   topupInput
	calls      int
}

func (f *fakeTopupStore) Topup(_ context.Context, accountID, createdBy string, in topupInput) (topupResult, error) {
	f.calls++
	f.gotAccount = accountID
	f.gotUser = createdBy
	f.gotInput = in
	return f.result, f.err
}

func (f *fakeTopupStore) TopupHistory(_ context.Context, accountID string) (topupBalanceResponse, error) {
	f.gotAccount = accountID
	return f.history, f.err
}

func topupReq(method, body string, claims *auth.Claims) *http.Request {
	req := httptest.NewRequest(method, "/v1/api/billing/topup", strings.NewReader(body))
	if claims != nil {
		req = withClaims(req, claims)
	}
	return req
}

func TestTopupHandler(t *testing.T) {
	adv := &auth.Claims{AccountID: "11111111-1111-4111-8111-111111111111", UserID: "user-1", Permissions: []string{"billing:view", "billing:topup"}}

	// GET history, scoped.
	store := &fakeTopupStore{history: topupBalanceResponse{Balance: 250, Currency: "USD", Topups: []topupView{{ID: "t1", Amount: 250}}}}
	rec := httptest.NewRecorder()
	topupHandler(store, nil, quietLog())(rec, topupReq(http.MethodGet, "", adv))
	if rec.Code != http.StatusOK || store.gotAccount != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("get code=%d account=%q", rec.Code, store.gotAccount)
	}

	// POST valid → 201, scoped + parsed, INFO trail not asserted.
	store = &fakeTopupStore{result: topupResult{ID: "t2", Amount: 500, Currency: "USD", Status: "succeeded", Balance: 750}}
	rec = httptest.NewRecorder()
	topupHandler(store, nil, quietLog())(rec, topupReq(http.MethodPost, `{"amount":500,"idempotency_key":"key-1"}`, adv))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create code = %d, want 201; body %s", rec.Code, rec.Body.String())
	}
	if store.gotAccount != "11111111-1111-4111-8111-111111111111" || store.gotUser != "user-1" || store.gotInput.IdempotencyKey != "key-1" {
		t.Errorf("create not scoped/parsed: %q %q %+v", store.gotAccount, store.gotUser, store.gotInput)
	}
	if store.gotInput.Currency != "USD" {
		t.Errorf("currency default = %q, want USD", store.gotInput.Currency)
	}

	// Idempotent replay → 200 (not 201) with duplicate flag.
	store = &fakeTopupStore{result: topupResult{ID: "t2", Amount: 500, Status: "succeeded", Balance: 750, Duplicate: true}}
	rec = httptest.NewRecorder()
	topupHandler(store, nil, quietLog())(rec, topupReq(http.MethodPost, `{"amount":500,"idempotency_key":"key-1"}`, adv))
	if rec.Code != http.StatusOK {
		t.Errorf("replay code = %d, want 200", rec.Code)
	}
	var res topupResult
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if !res.Duplicate {
		t.Error("replay response missing duplicate flag")
	}

	// Key reused with different amount → 409.
	store = &fakeTopupStore{err: errTopupKeyReused}
	rec = httptest.NewRecorder()
	topupHandler(store, nil, quietLog())(rec, topupReq(http.MethodPost, `{"amount":900,"idempotency_key":"key-1"}`, adv))
	if rec.Code != http.StatusConflict {
		t.Errorf("key reuse code = %d, want 409", rec.Code)
	}

	// Missing idempotency key → 400, store never called.
	store = &fakeTopupStore{}
	rec = httptest.NewRecorder()
	topupHandler(store, nil, quietLog())(rec, topupReq(http.MethodPost, `{"amount":500}`, adv))
	if rec.Code != http.StatusBadRequest || store.calls != 0 {
		t.Errorf("no key: code=%d calls=%d, want 400/0", rec.Code, store.calls)
	}

	// Zero, negative, and over-max amounts → 400, store never called.
	for _, body := range []string{`{"amount":0,"idempotency_key":"k"}`, `{"amount":-5,"idempotency_key":"k"}`, `{"amount":10001,"idempotency_key":"k"}`} {
		store = &fakeTopupStore{}
		rec = httptest.NewRecorder()
		topupHandler(store, nil, quietLog())(rec, topupReq(http.MethodPost, body, adv))
		if rec.Code != http.StatusBadRequest || store.calls != 0 {
			t.Errorf("body %s: code=%d calls=%d, want 400/0", body, rec.Code, store.calls)
		}
	}

	// billing:view only → can read, cannot topup.
	viewer := &auth.Claims{AccountID: "11111111-1111-4111-8111-111111111111", Permissions: []string{"billing:view"}}
	rec = httptest.NewRecorder()
	topupHandler(&fakeTopupStore{}, nil, quietLog())(rec, topupReq(http.MethodPost, `{"amount":5,"idempotency_key":"k"}`, viewer))
	if rec.Code != http.StatusForbidden {
		t.Errorf("viewer topup code = %d, want 403", rec.Code)
	}

	// No claims → 401.
	rec = httptest.NewRecorder()
	topupHandler(&fakeTopupStore{}, nil, quietLog())(rec, topupReq(http.MethodGet, "", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no claims code = %d, want 401", rec.Code)
	}

	// Dev-bypass identity (non-UUID account): GET serves an empty view
	// instead of a uuid-cast 500; POST refuses the credit; store untouched.
	dev := &auth.Claims{AccountID: "dev-account", Permissions: []string{"*"}}
	store = &fakeTopupStore{}
	rec = httptest.NewRecorder()
	topupHandler(store, nil, quietLog())(rec, topupReq(http.MethodGet, "", dev))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"topups":[]`) {
		t.Errorf("dev GET: code=%d body=%s, want 200 empty view", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	topupHandler(store, nil, quietLog())(rec, topupReq(http.MethodPost, `{"amount":5,"idempotency_key":"k"}`, dev))
	if rec.Code != http.StatusBadRequest || store.calls != 0 {
		t.Errorf("dev POST: code=%d calls=%d, want 400/0", rec.Code, store.calls)
	}
}
