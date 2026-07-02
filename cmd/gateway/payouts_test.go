package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
)

type fakePayoutStore struct {
	resp   payoutsResponse
	gotAcc string
}

func (f *fakePayoutStore) ListPayouts(_ context.Context, accountID string) (payoutsResponse, error) {
	f.gotAcc = accountID
	return f.resp, nil
}

func TestPayoutsHandler(t *testing.T) {
	pub := &auth.Claims{AccountID: "acc-9", AccountType: auth.AccountPublisher,
		Permissions: []string{"earnings:view"}}

	// GET scoped to caller's account, rollup echoed through.
	store := &fakePayoutStore{resp: payoutsResponse{
		Payouts:      []payoutView{{ID: "p1", Status: "paid", Amount: 12.5}},
		PaidCents:    1250,
		PendingCents: 0,
		Currency:     "USD",
	}}
	rec := httptest.NewRecorder()
	req := withClaims(httptest.NewRequest(http.MethodGet, "/v1/api/payouts", nil), pub)
	payoutsHandler(store, quietLog())(rec, req)
	if rec.Code != http.StatusOK || store.gotAcc != "acc-9" {
		t.Fatalf("get code=%d acc=%q body=%s", rec.Code, store.gotAcc, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"paid_cents":1250`) {
		t.Errorf("rollup missing from body: %s", rec.Body.String())
	}

	// POST not allowed (read-only).
	rec = httptest.NewRecorder()
	req = withClaims(httptest.NewRequest(http.MethodPost, "/v1/api/payouts", nil), pub)
	payoutsHandler(&fakePayoutStore{}, quietLog())(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("post code = %d, want 405", rec.Code)
	}

	// Missing perm → 403.
	rec = httptest.NewRecorder()
	req = withClaims(httptest.NewRequest(http.MethodGet, "/v1/api/payouts", nil), &auth.Claims{Permissions: []string{"campaigns:read"}})
	payoutsHandler(&fakePayoutStore{}, quietLog())(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("no perm code = %d, want 403", rec.Code)
	}

	// No claims → 401.
	rec = httptest.NewRecorder()
	payoutsHandler(&fakePayoutStore{}, quietLog())(rec, httptest.NewRequest(http.MethodGet, "/v1/api/payouts", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no claims code = %d, want 401", rec.Code)
	}
}
