package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
)

type fakeBillingTermsStore struct {
	view    billingTermsView
	got     billingTermsView
	gotSet  bool
	created bool // records that SetTerms was reached (upsert creates the row when absent)
}

func (f *fakeBillingTermsStore) TermsFor(_ context.Context, accountID string) (billingTermsView, error) {
	v := f.view
	if v.AccountID == "" {
		v = billingTermsView{AccountID: accountID, PaymentTerms: "prepay"}
	}
	return v, nil
}

func (f *fakeBillingTermsStore) SetTerms(_ context.Context, accountID, _ string, in billingTermsView) error {
	f.got = in
	f.gotSet = true
	f.created = true // the pg store UPSERTs — a missing row is created at balance 0
	return nil
}

const btAcct = "aaaaaaa4-4444-4444-8444-444444444444"

func btReq(method, url string, body any, claims *auth.Claims) *http.Request {
	var r *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		r = httptest.NewRequest(method, url, bytes.NewReader(b))
	} else {
		r = httptest.NewRequest(method, url, nil)
	}
	if claims != nil {
		r = withClaims(r, claims)
	}
	return r
}

func TestBillingTerms_PutRequiresSupportUpdate(t *testing.T) {
	store := &fakeBillingTermsStore{}
	h := billingTermsHandler(store, nil, quietLog())

	// An advertiser/tenant session (no support:update) → 403, no write.
	advertiser := &auth.Claims{AccountID: btAcct, Permissions: []string{"billing:view", "billing:topup", "campaigns:read"}}
	rec := httptest.NewRecorder()
	h(rec, btReq(http.MethodPut, "/v1/api/billing/terms",
		billingTermsView{AccountID: btAcct, PaymentTerms: "invoiced", CreditLimit: 5000}, advertiser))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("advertiser PUT code = %d, want 403", rec.Code)
	}
	if store.gotSet {
		t.Fatal("advertiser PUT must not reach the store")
	}

	// support:update → 200, row upserted (created when absent).
	staff := &auth.Claims{AccountID: "staff-1", UserID: "staff-user", Permissions: []string{"support:read", "support:update"}}
	rec = httptest.NewRecorder()
	h(rec, btReq(http.MethodPut, "/v1/api/billing/terms",
		billingTermsView{AccountID: btAcct, PaymentTerms: "invoiced", CreditLimit: 5000}, staff))
	if rec.Code != http.StatusOK {
		t.Fatalf("staff PUT code = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if !store.created {
		t.Fatal("staff PUT must upsert the terms (creating the row if absent)")
	}
	if store.got.PaymentTerms != "invoiced" || store.got.CreditLimit != 5000 {
		t.Errorf("stored terms = %+v, want invoiced/5000", store.got)
	}
}

func TestBillingTerms_PutValidates(t *testing.T) {
	staff := &auth.Claims{AccountID: "staff-1", UserID: "staff-user", Permissions: []string{"support:read", "support:update"}}

	cases := []struct {
		name string
		body billingTermsView
	}{
		{"bad enum", billingTermsView{AccountID: btAcct, PaymentTerms: "net_30", CreditLimit: 100}},
		{"empty enum", billingTermsView{AccountID: btAcct, PaymentTerms: "", CreditLimit: 100}},
		{"negative credit", billingTermsView{AccountID: btAcct, PaymentTerms: "invoiced", CreditLimit: -1}},
		{"bad account id", billingTermsView{AccountID: "not-a-uuid", PaymentTerms: "invoiced", CreditLimit: 100}},
	}
	for _, c := range cases {
		store := &fakeBillingTermsStore{}
		rec := httptest.NewRecorder()
		billingTermsHandler(store, nil, quietLog())(rec, btReq(http.MethodPut, "/v1/api/billing/terms", c.body, staff))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: code = %d, want 400", c.name, rec.Code)
		}
		if store.gotSet {
			t.Errorf("%s: invalid PUT must not reach the store", c.name)
		}
	}

	// prepay normalizes credit_limit to 0 regardless of the submitted value.
	store := &fakeBillingTermsStore{}
	rec := httptest.NewRecorder()
	billingTermsHandler(store, nil, quietLog())(rec, btReq(http.MethodPut, "/v1/api/billing/terms",
		billingTermsView{AccountID: btAcct, PaymentTerms: "prepay", CreditLimit: 999}, staff))
	if rec.Code != http.StatusOK {
		t.Fatalf("prepay PUT code = %d, want 200", rec.Code)
	}
	if store.got.CreditLimit != 0 {
		t.Errorf("prepay credit_limit = %v, want normalized to 0", store.got.CreditLimit)
	}
}

func TestBillingTerms_Get(t *testing.T) {
	store := &fakeBillingTermsStore{view: billingTermsView{AccountID: btAcct, PaymentTerms: "invoiced", CreditLimit: 2500}}
	h := billingTermsHandler(store, nil, quietLog())

	// support:read → 200 with the terms.
	staff := &auth.Claims{AccountID: "staff-1", Permissions: []string{"support:read"}}
	rec := httptest.NewRecorder()
	h(rec, btReq(http.MethodGet, "/v1/api/billing/terms?account_id="+btAcct, nil, staff))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET code = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var got billingTermsView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.PaymentTerms != "invoiced" || got.CreditLimit != 2500 {
		t.Errorf("GET body = %+v, want invoiced/2500", got)
	}

	// Missing/invalid account_id → 400.
	rec = httptest.NewRecorder()
	h(rec, btReq(http.MethodGet, "/v1/api/billing/terms", nil, staff))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("GET without account_id code = %d, want 400", rec.Code)
	}

	// No support:read → 403.
	rec = httptest.NewRecorder()
	h(rec, btReq(http.MethodGet, "/v1/api/billing/terms?account_id="+btAcct, nil,
		&auth.Claims{Permissions: []string{"billing:view"}}))
	if rec.Code != http.StatusForbidden {
		t.Errorf("no perm GET code = %d, want 403", rec.Code)
	}

	// No claims → 401.
	rec = httptest.NewRecorder()
	h(rec, btReq(http.MethodGet, "/v1/api/billing/terms?account_id="+btAcct, nil, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no claims code = %d, want 401", rec.Code)
	}
}
