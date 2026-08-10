package main

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
)

type fakeFraudStore struct {
	entries  []blocklistEntry
	added    [3]string // type, value, reason
	deleted  string
	notFound bool
}

func (f *fakeFraudStore) ListBlocklist(context.Context) ([]blocklistEntry, error) {
	return f.entries, nil
}
func (f *fakeFraudStore) AddBlocklist(_ context.Context, typ, value, reason string) (string, error) {
	f.added = [3]string{typ, value, reason}
	return "bl-1", nil
}
func (f *fakeFraudStore) DeleteBlocklist(_ context.Context, id string) error {
	if f.notFound {
		return sql.ErrNoRows
	}
	f.deleted = id
	return nil
}

// countingBus records the last invalidate subject published.
type countingBus struct {
	events.EventBus
	published int
	subject   string
}

func (b *countingBus) Publish(_ context.Context, subject string, _ []byte) error {
	b.published++
	b.subject = subject
	return nil
}

func fraudReq(method, target, body string, claims *auth.Claims) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if claims != nil {
		req = withClaims(req, claims)
	}
	return req
}

func TestFraudRulesHandler(t *testing.T) {
	staff := &auth.Claims{UserID: "user-1", AccountType: auth.AccountStaff, Permissions: []string{"fraud:read", "fraud:update"}}

	// List.
	store := &fakeFraudStore{entries: []blocklistEntry{{ID: "b1", Type: "ip", Value: "1.2.3.4"}}}
	rec := httptest.NewRecorder()
	fraudRulesHandler(store, nil, nil, quietLog())(rec, fraudReq(http.MethodGet, fraudPath, "", staff))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "1.2.3.4") {
		t.Fatalf("list code=%d body=%s", rec.Code, rec.Body.String())
	}

	// Add → publishes invalidate.
	store = &fakeFraudStore{}
	bus := &countingBus{}
	rec = httptest.NewRecorder()
	fraudRulesHandler(store, bus, nil, quietLog())(rec, fraudReq(http.MethodPost, fraudPath, `{"type":"domain","value":"bad.example","reason":"spam"}`, staff))
	if rec.Code != http.StatusCreated || store.added != [3]string{"domain", "bad.example", "spam"} {
		t.Errorf("add code=%d added=%v", rec.Code, store.added)
	}
	if bus.published != 1 || bus.subject != events.SubjectCacheInvalidateFraudRules {
		t.Errorf("add invalidate: published=%d subject=%q", bus.published, bus.subject)
	}

	// Add with bad type → 400.
	rec = httptest.NewRecorder()
	fraudRulesHandler(&fakeFraudStore{}, nil, nil, quietLog())(rec, fraudReq(http.MethodPost, fraudPath, `{"type":"nope","value":"x"}`, staff))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad type code = %d, want 400", rec.Code)
	}

	// Delete.
	store = &fakeFraudStore{}
	bus = &countingBus{}
	rec = httptest.NewRecorder()
	fraudRulesHandler(store, bus, nil, quietLog())(rec, fraudReq(http.MethodDelete, fraudPath+"?id=b1", "", staff))
	if rec.Code != http.StatusOK || store.deleted != "b1" || bus.published != 1 {
		t.Errorf("delete code=%d deleted=%q published=%d", rec.Code, store.deleted, bus.published)
	}

	// Delete unknown → 404.
	rec = httptest.NewRecorder()
	fraudRulesHandler(&fakeFraudStore{notFound: true}, nil, nil, quietLog())(rec, fraudReq(http.MethodDelete, fraudPath+"?id=zzz", "", staff))
	if rec.Code != http.StatusNotFound {
		t.Errorf("delete-unknown code = %d, want 404", rec.Code)
	}

	// Missing fraud perm → 403.
	rec = httptest.NewRecorder()
	fraudRulesHandler(&fakeFraudStore{}, nil, nil, quietLog())(rec, fraudReq(http.MethodGet, fraudPath, "", &auth.Claims{Permissions: []string{"campaigns:read"}}))
	if rec.Code != http.StatusForbidden {
		t.Errorf("no perm code = %d, want 403", rec.Code)
	}
}

// fraudPath is the fraud blocklist endpoint; kept terse for the test requests.
const fraudPath = "/v1/api/fraud/blocklists"
