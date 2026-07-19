package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
)

// fakeInvoiceStore is an in-memory invoiceStore keyed by account so the tests
// can assert tenant scoping (a cross-tenant id is invisible → 404).
type fakeInvoiceStore struct {
	list   map[string]invoicesResponse         // account -> list
	detail map[string]map[string]invoiceDetail // account -> id -> detail
	gotAcc string
}

func (f *fakeInvoiceStore) ListInvoices(_ context.Context, accountID string) (invoicesResponse, error) {
	f.gotAcc = accountID
	if r, ok := f.list[accountID]; ok {
		return r, nil
	}
	return invoicesResponse{Invoices: []invoiceView{}}, nil
}

func (f *fakeInvoiceStore) GetInvoice(_ context.Context, accountID, invoiceID string) (invoiceDetail, bool, error) {
	f.gotAcc = accountID
	if byID, ok := f.detail[accountID]; ok {
		if d, ok := byID[invoiceID]; ok {
			return d, true, nil
		}
	}
	return invoiceDetail{Lines: []invoiceLineView{}}, false, nil
}

const (
	invAcct  = "11111111-1111-4111-8111-111111111111"
	invOther = "22222222-2222-4222-8222-222222222222"
)

func invClaims(accountID string, perms ...string) *auth.Claims {
	return &auth.Claims{UserID: "u1", AccountID: accountID, AccountType: auth.AccountAdvertiser, Permissions: perms}
}

func TestInvoicesHandler_ListScoped(t *testing.T) {
	store := &fakeInvoiceStore{list: map[string]invoicesResponse{
		invAcct: {Invoices: []invoiceView{{ID: "inv1", Total: 42.5, Currency: "USD", Status: "pending"}}},
	}}
	rec := httptest.NewRecorder()
	req := withClaims(httptest.NewRequest(http.MethodGet, "/v1/api/invoices", nil), invClaims(invAcct, "billing:view"))
	invoicesHandler(store, quietLog())(rec, req)

	if rec.Code != http.StatusOK || store.gotAcc != invAcct {
		t.Fatalf("list code=%d acc=%q body=%s", rec.Code, store.gotAcc, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"inv1"`) || !strings.Contains(rec.Body.String(), `42.5`) {
		t.Errorf("invoice missing from body: %s", rec.Body.String())
	}
}

func TestInvoicesHandler_EmptyState(t *testing.T) {
	store := &fakeInvoiceStore{}
	rec := httptest.NewRecorder()
	req := withClaims(httptest.NewRequest(http.MethodGet, "/v1/api/invoices", nil), invClaims(invAcct, "billing:view"))
	invoicesHandler(store, quietLog())(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("empty code = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"invoices":[]`) {
		t.Errorf("want empty invoices array, got %s", rec.Body.String())
	}
}

func TestInvoicesHandler_Detail(t *testing.T) {
	store := &fakeInvoiceStore{detail: map[string]map[string]invoiceDetail{
		invAcct: {"inv1": {
			invoiceView: invoiceView{ID: "inv1", Total: 42.5, Currency: "USD", Status: "pending"},
			Lines:       []invoiceLineView{{CampaignID: "c1", CampaignName: "Summer", Spend: 42.5, BidModel: "cpm"}},
		}},
	}}
	rec := httptest.NewRecorder()
	req := withClaims(httptest.NewRequest(http.MethodGet, "/v1/api/invoices/inv1", nil), invClaims(invAcct, "billing:view"))
	invoicesHandler(store, quietLog())(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("detail code = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"Summer"`) || !strings.Contains(rec.Body.String(), `"lines"`) {
		t.Errorf("line items missing: %s", rec.Body.String())
	}
}

// A cross-tenant id must 404 (looks identical to a missing invoice).
func TestInvoicesHandler_DetailCrossTenant404(t *testing.T) {
	store := &fakeInvoiceStore{detail: map[string]map[string]invoiceDetail{
		invOther: {"inv1": {invoiceView: invoiceView{ID: "inv1"}}},
	}}
	rec := httptest.NewRecorder()
	// Caller is invAcct but the invoice belongs to invOther.
	req := withClaims(httptest.NewRequest(http.MethodGet, "/v1/api/invoices/inv1", nil), invClaims(invAcct, "billing:view"))
	invoicesHandler(store, quietLog())(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant code = %d, want 404 body=%s", rec.Code, rec.Body.String())
	}
}

func TestInvoicesHandler_RequiresBillingView(t *testing.T) {
	rec := httptest.NewRecorder()
	req := withClaims(httptest.NewRequest(http.MethodGet, "/v1/api/invoices", nil), invClaims(invAcct, "campaigns:read"))
	invoicesHandler(&fakeInvoiceStore{}, quietLog())(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("no perm code = %d, want 403", rec.Code)
	}
}

func TestInvoicesHandler_MethodAndAuth(t *testing.T) {
	// POST not allowed (read-only).
	rec := httptest.NewRecorder()
	req := withClaims(httptest.NewRequest(http.MethodPost, "/v1/api/invoices", nil), invClaims(invAcct, "billing:view"))
	invoicesHandler(&fakeInvoiceStore{}, quietLog())(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("post code = %d, want 405", rec.Code)
	}

	// No claims → 401.
	rec = httptest.NewRecorder()
	invoicesHandler(&fakeInvoiceStore{}, quietLog())(rec, httptest.NewRequest(http.MethodGet, "/v1/api/invoices", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no claims code = %d, want 401", rec.Code)
	}
}
