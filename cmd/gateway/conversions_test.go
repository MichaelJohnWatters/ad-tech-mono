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

// fakeConversionStore is an in-memory conversionStore that records the account
// it was called with and only ever returns rows for that account — so the tests
// can assert tenant scoping without a database.
type fakeConversionStore struct {
	rows      map[string][]conversionConfig // account_id -> configs
	gotList   string
	gotCreate string
	gotDelete string
	nextID    string
}

func (f *fakeConversionStore) List(_ context.Context, accountID string) ([]conversionConfig, error) {
	f.gotList = accountID
	return append([]conversionConfig(nil), f.rows[accountID]...), nil
}

func (f *fakeConversionStore) Create(_ context.Context, accountID string, in conversionInput) (conversionConfig, error) {
	f.gotCreate = accountID
	id := f.nextID
	if id == "" {
		id = "11111111-1111-1111-1111-111111111111"
	}
	c := conversionConfig{
		ID: id, Name: in.Name, EventType: in.EventType,
		DefaultValue: in.DefaultValue, Currency: in.Currency, Status: "active",
	}
	if f.rows == nil {
		f.rows = map[string][]conversionConfig{}
	}
	f.rows[accountID] = append(f.rows[accountID], c)
	return c, nil
}

func (f *fakeConversionStore) Delete(_ context.Context, accountID, id string) error {
	f.gotDelete = accountID
	kept := f.rows[accountID][:0]
	found := false
	for _, c := range f.rows[accountID] {
		if c.ID == id {
			found = true
			continue
		}
		kept = append(kept, c)
	}
	if !found {
		return sql.ErrNoRows
	}
	f.rows[accountID] = kept
	return nil
}

const (
	convAcct  = "aaaaaaaa-1111-1111-1111-111111111111"
	convOther = "bbbbbbbb-2222-2222-2222-222222222222"
)

func convClaims(accountID string, perms ...string) *auth.Claims {
	return &auth.Claims{AccountID: accountID, AccountType: auth.AccountAdvertiser, Permissions: perms}
}

const testTrackerURL = "https://track.example.com"

func TestConversionsHandler_ListTenantScoped(t *testing.T) {
	store := &fakeConversionStore{rows: map[string][]conversionConfig{
		convAcct:  {{ID: "c1", Name: "Purchase", EventType: "purchase", DefaultValue: 9.99, Currency: "USD", Status: "active"}},
		convOther: {{ID: "c2", Name: "Other", EventType: "lead", Currency: "USD", Status: "active"}},
	}}
	rec := httptest.NewRecorder()
	req := withClaims(httptest.NewRequest(http.MethodGet, "/v1/api/conversions", nil), convClaims(convAcct, "campaigns:read"))
	conversionsHandler(store, testTrackerURL, quietLog())(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET code = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if store.gotList != convAcct {
		t.Errorf("List scoped to %q, want %q", store.gotList, convAcct)
	}
	var got []conversionConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].Name != "Purchase" {
		t.Fatalf("got %d rows, want only the caller's one Purchase config: %+v", len(got), got)
	}
	// The pixel must carry the config's type + params and point at the tracker.
	px := got[0].Pixel
	if !strings.Contains(px, "type=purchase") || !strings.Contains(px, "rev=9.99") ||
		!strings.Contains(px, "cur=USD") || !strings.Contains(px, "__TRACE_ID__") ||
		!strings.Contains(px, testTrackerURL+"/v1/t/conv") {
		t.Errorf("pixel missing expected params/tracker: %s", px)
	}
	if !strings.Contains(got[0].Snippet, "new Image") || !strings.Contains(got[0].Snippet, "type=purchase") {
		t.Errorf("snippet missing JS beacon/params: %s", got[0].Snippet)
	}
}

func TestConversionsHandler_CreateBindsCallerAccount(t *testing.T) {
	store := &fakeConversionStore{nextID: "33333333-3333-3333-3333-333333333333"}
	body := `{"account_id":"` + convOther + `","name":"Signup","event_type":"signup","default_value":5,"currency":"EUR"}`
	rec := httptest.NewRecorder()
	req := withClaims(httptest.NewRequest(http.MethodPost, "/v1/api/conversions", strings.NewReader(body)),
		convClaims(convAcct, "campaigns:create"))
	conversionsHandler(store, testTrackerURL, quietLog())(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("POST code = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	// A body account_id must be ignored — the row binds to the caller.
	if store.gotCreate != convAcct {
		t.Errorf("Create bound to %q, want caller %q", store.gotCreate, convAcct)
	}
	var got conversionConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.EventType != "signup" || got.Currency != "EUR" {
		t.Errorf("created config = %+v", got)
	}
	if !strings.Contains(got.Pixel, "type=signup") || !strings.Contains(got.Pixel, "cur=EUR") {
		t.Errorf("create response pixel wrong: %s", got.Pixel)
	}
}

func TestConversionsHandler_CrossTenantDeleteIsolated(t *testing.T) {
	// convOther owns c2; a delete from convAcct must not touch it (fake scopes
	// Delete by account, returning ErrNoRows → 404).
	store := &fakeConversionStore{rows: map[string][]conversionConfig{
		convOther: {{ID: "c2", Name: "Other", EventType: "lead", Currency: "USD", Status: "active"}},
	}}
	rec := httptest.NewRecorder()
	req := withClaims(httptest.NewRequest(http.MethodDelete, "/v1/api/conversions?id=c2", nil),
		convClaims(convAcct, "campaigns:delete"))
	conversionsHandler(store, testTrackerURL, quietLog())(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant DELETE code = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	if store.gotDelete != convAcct {
		t.Errorf("Delete scoped to %q, want %q", store.gotDelete, convAcct)
	}
	if len(store.rows[convOther]) != 1 {
		t.Errorf("other tenant's config was removed: %+v", store.rows[convOther])
	}
}

func TestConversionsHandler_Permissions(t *testing.T) {
	store := &fakeConversionStore{}
	// GET without campaigns:read → 403.
	rec := httptest.NewRecorder()
	conversionsHandler(store, testTrackerURL, quietLog())(rec,
		withClaims(httptest.NewRequest(http.MethodGet, "/v1/api/conversions", nil), convClaims(convAcct)))
	if rec.Code != http.StatusForbidden {
		t.Errorf("GET no-perm code = %d, want 403", rec.Code)
	}
	// POST without campaigns:create → 403.
	rec = httptest.NewRecorder()
	conversionsHandler(store, testTrackerURL, quietLog())(rec,
		withClaims(httptest.NewRequest(http.MethodPost, "/v1/api/conversions", strings.NewReader(`{"name":"x"}`)),
			convClaims(convAcct, "campaigns:read")))
	if rec.Code != http.StatusForbidden {
		t.Errorf("POST no-perm code = %d, want 403", rec.Code)
	}
	// No claims → 401.
	rec = httptest.NewRecorder()
	conversionsHandler(store, testTrackerURL, quietLog())(rec,
		httptest.NewRequest(http.MethodGet, "/v1/api/conversions", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no-claims code = %d, want 401", rec.Code)
	}
}
