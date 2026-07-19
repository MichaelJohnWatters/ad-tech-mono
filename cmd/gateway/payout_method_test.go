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

// fakePayoutMethodStore is an in-memory payoutMethodStore that scopes every
// call by account and records the raw detail it was handed — so the tests can
// assert tenant isolation and that raw details never leak back out of the API.
type fakePayoutMethodStore struct {
	rows   map[string]payoutMethod // account_id -> configured method
	rawGot map[string]string       // account_id -> last raw detail written
}

func (f *fakePayoutMethodStore) Get(_ context.Context, accountID string) (payoutMethod, bool, error) {
	m, ok := f.rows[accountID]
	return m, ok, nil
}

func (f *fakePayoutMethodStore) Upsert(_ context.Context, accountID string, in payoutMethodInput, rawDetail string) (payoutMethod, error) {
	if rawDetail == "" {
		if _, ok := f.rows[accountID]; !ok {
			return payoutMethod{}, errPayoutDetailRequired
		}
	}
	if f.rows == nil {
		f.rows = map[string]payoutMethod{}
	}
	if f.rawGot == nil {
		f.rawGot = map[string]string{}
	}
	f.rawGot[accountID] = rawDetail
	m := f.rows[accountID]
	m.MethodType = in.MethodType
	m.DisplayName = in.DisplayName
	m.MinimumPayoutCents = in.MinimumPayoutCents
	m.Currency = in.Currency
	m.Status = "active"
	if rawDetail != "" {
		m.Last4 = tailOf(in.MethodType, rawDetail)
	}
	f.rows[accountID] = m
	return m, nil
}

const (
	pmAcct  = "aaaaaaaa-1111-4111-8111-111111111111"
	pmOther = "bbbbbbbb-2222-4222-8222-222222222222"
)

func pmClaims(accountID string, perms ...string) *auth.Claims {
	return &auth.Claims{UserID: "u1", AccountID: accountID, AccountType: auth.AccountPublisher, Permissions: perms}
}

// GET masks details: the raw account number must never appear in the response.
func TestPayoutMethodHandler_GetMasksDetails(t *testing.T) {
	store := &fakePayoutMethodStore{rows: map[string]payoutMethod{
		pmAcct: {MethodType: "bank_transfer", DisplayName: "Main", Last4: "6789",
			MinimumPayoutCents: 5000, Currency: "USD", Status: "active"},
	}}
	rec := httptest.NewRecorder()
	req := withClaims(httptest.NewRequest(http.MethodGet, "/v1/api/payout-method", nil),
		pmClaims(pmAcct, "earnings:view"))
	payoutMethodHandler(store, quietLog())(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET code = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "account_number") || strings.Contains(strings.ToLower(body), "details") {
		t.Fatalf("response leaked raw details: %s", body)
	}
	var got payoutMethodView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Configured || got.MaskedTail != "•••• 6789" {
		t.Fatalf("want configured masked tail '•••• 6789', got %+v", got)
	}
	if got.MethodType != "bank_transfer" || got.MinimumPayoutCents != 5000 {
		t.Errorf("read view wrong: %+v", got)
	}
}

// A PayPal email is masked, not returned whole.
func TestPayoutMethodHandler_GetMasksPaypalEmail(t *testing.T) {
	store := &fakePayoutMethodStore{rows: map[string]payoutMethod{
		pmAcct: {MethodType: "paypal", Last4: "jane@example.com", Currency: "USD", Status: "active"},
	}}
	rec := httptest.NewRecorder()
	req := withClaims(httptest.NewRequest(http.MethodGet, "/v1/api/payout-method", nil),
		pmClaims(pmAcct, "earnings:view"))
	payoutMethodHandler(store, quietLog())(rec, req)

	var got payoutMethodView
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.MaskedTail != "j***@example.com" {
		t.Fatalf("paypal email not masked: %q", got.MaskedTail)
	}
}

// No configured method → empty (unconfigured) shape, not an error.
func TestPayoutMethodHandler_GetUnconfigured(t *testing.T) {
	store := &fakePayoutMethodStore{}
	rec := httptest.NewRecorder()
	req := withClaims(httptest.NewRequest(http.MethodGet, "/v1/api/payout-method", nil),
		pmClaims(pmAcct, "earnings:view"))
	payoutMethodHandler(store, quietLog())(rec, req)

	var got payoutMethodView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Configured {
		t.Fatalf("want unconfigured, got %+v", got)
	}
}

// PUT round-trips: upsert stores, response is masked, raw detail reached the store.
func TestPayoutMethodHandler_PutUpsertRoundTrip(t *testing.T) {
	store := &fakePayoutMethodStore{}
	body := `{"method_type":"bank_transfer","display_name":"Main","details":{"account_number":"12345678"},"minimum_payout_cents":5000,"currency":"USD"}`
	rec := httptest.NewRecorder()
	req := withClaims(httptest.NewRequest(http.MethodPut, "/v1/api/payout-method", strings.NewReader(body)),
		pmClaims(pmAcct, "earnings:manage"))
	payoutMethodHandler(store, quietLog())(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("PUT code = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if store.rawGot[pmAcct] != "12345678" {
		t.Errorf("store got raw detail %q, want 12345678", store.rawGot[pmAcct])
	}
	if strings.Contains(rec.Body.String(), "12345678") {
		t.Fatalf("PUT response leaked raw account number: %s", rec.Body.String())
	}
	var got payoutMethodView
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.MaskedTail != "•••• 5678" || got.MinimumPayoutCents != 5000 {
		t.Fatalf("PUT view wrong: %+v", got)
	}
}

// PUT rejects an unknown method type.
func TestPayoutMethodHandler_PutRejectsBadType(t *testing.T) {
	store := &fakePayoutMethodStore{}
	body := `{"method_type":"crypto","details":{"account_number":"1"},"minimum_payout_cents":0}`
	rec := httptest.NewRecorder()
	req := withClaims(httptest.NewRequest(http.MethodPut, "/v1/api/payout-method", strings.NewReader(body)),
		pmClaims(pmAcct, "earnings:manage"))
	payoutMethodHandler(store, quietLog())(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad type code = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
}

// PUT rejects a negative threshold.
func TestPayoutMethodHandler_PutRejectsNegativeThreshold(t *testing.T) {
	store := &fakePayoutMethodStore{}
	body := `{"method_type":"paypal","details":{"paypal_email":"a@b.com"},"minimum_payout_cents":-1}`
	rec := httptest.NewRecorder()
	req := withClaims(httptest.NewRequest(http.MethodPut, "/v1/api/payout-method", strings.NewReader(body)),
		pmClaims(pmAcct, "earnings:manage"))
	payoutMethodHandler(store, quietLog())(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("negative threshold code = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
}

// PUT without a destination on a first-time config → 400 (detail required).
func TestPayoutMethodHandler_PutFirstTimeRequiresDetail(t *testing.T) {
	store := &fakePayoutMethodStore{}
	body := `{"method_type":"bank_transfer","minimum_payout_cents":100}`
	rec := httptest.NewRecorder()
	req := withClaims(httptest.NewRequest(http.MethodPut, "/v1/api/payout-method", strings.NewReader(body)),
		pmClaims(pmAcct, "earnings:manage"))
	payoutMethodHandler(store, quietLog())(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing detail code = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
}

// PUT requires earnings:manage — earnings:view alone is 403.
func TestPayoutMethodHandler_PutRequiresManage(t *testing.T) {
	store := &fakePayoutMethodStore{}
	body := `{"method_type":"paypal","details":{"paypal_email":"a@b.com"},"minimum_payout_cents":0}`
	rec := httptest.NewRecorder()
	req := withClaims(httptest.NewRequest(http.MethodPut, "/v1/api/payout-method", strings.NewReader(body)),
		pmClaims(pmAcct, "earnings:view")) // read-only
	payoutMethodHandler(store, quietLog())(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("PUT without manage = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	if _, wrote := store.rawGot[pmAcct]; wrote {
		t.Errorf("forbidden PUT still reached the store")
	}
}

// Cross-tenant GET returns only the caller's method.
func TestPayoutMethodHandler_CrossTenantIsolated(t *testing.T) {
	store := &fakePayoutMethodStore{rows: map[string]payoutMethod{
		pmOther: {MethodType: "wire", Last4: "9999", Currency: "USD", Status: "active"},
	}}
	rec := httptest.NewRecorder()
	req := withClaims(httptest.NewRequest(http.MethodGet, "/v1/api/payout-method", nil),
		pmClaims(pmAcct, "earnings:view")) // caller has no method; other tenant does
	payoutMethodHandler(store, quietLog())(rec, req)

	var got payoutMethodView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Configured {
		t.Fatalf("caller saw another tenant's method: %+v", got)
	}
}
