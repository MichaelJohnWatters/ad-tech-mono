package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
)

type fakePublisherLookup struct {
	ids []string
	err error
}

func (f fakePublisherLookup) PublisherIDs(_ context.Context, _ string) ([]string, error) {
	return f.ids, f.err
}

// capture reads the (possibly rewritten) body the middleware forwards.
func captureBody(t *testing.T, got *map[string]any) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if len(body) > 0 {
			if err := json.Unmarshal(body, got); err != nil {
				t.Fatalf("forwarded body not JSON: %v (%s)", err, body)
			}
		}
		w.WriteHeader(http.StatusOK)
	})
}

func reportReq(body string, claims *auth.Claims) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/api/reports", strings.NewReader(body))
	if claims != nil {
		req = withClaims(req, claims)
	}
	return req
}

func filtersOf(t *testing.T, q map[string]any) map[string]any {
	t.Helper()
	f, _ := q["filters"].(map[string]any)
	if f == nil {
		t.Fatalf("no filters in forwarded body: %v", q)
	}
	return f
}

func TestEnforceReportTenant(t *testing.T) {
	mw := enforceReportTenant(fakePublisherLookup{ids: []string{"pub-1"}}, quietLog())

	// Advertiser: account_id forced, even when the browser tried another one.
	var got map[string]any
	adv := &auth.Claims{AccountID: "adv-1", AccountType: auth.AccountAdvertiser}
	rec := httptest.NewRecorder()
	mw(captureBody(t, &got)).ServeHTTP(rec, reportReq(`{"table":"impressions","filters":{"account_id":"someone-else"}}`, adv))
	if rec.Code != http.StatusOK || filtersOf(t, got)["account_id"] != "adv-1" {
		t.Errorf("advertiser: code=%d filters=%v, want account_id forced to adv-1", rec.Code, got["filters"])
	}

	// Advertiser with no filters at all: filter injected.
	got = nil
	rec = httptest.NewRecorder()
	mw(captureBody(t, &got)).ServeHTTP(rec, reportReq(`{"table":"impressions"}`, adv))
	if filtersOf(t, got)["account_id"] != "adv-1" {
		t.Errorf("advertiser no-filters: %v, want account_id injected", got["filters"])
	}

	// Publisher with one publisher: publisher_id injected.
	pub := &auth.Claims{AccountID: "pubacc-1", AccountType: auth.AccountPublisher}
	got = nil
	rec = httptest.NewRecorder()
	mw(captureBody(t, &got)).ServeHTTP(rec, reportReq(`{"table":"auctions"}`, pub))
	if filtersOf(t, got)["publisher_id"] != "pub-1" {
		t.Errorf("publisher: %v, want publisher_id pub-1 injected", got["filters"])
	}

	// Publisher naming a publisher it doesn't own: 403.
	rec = httptest.NewRecorder()
	mw(captureBody(t, &got)).ServeHTTP(rec, reportReq(`{"filters":{"publisher_id":"pub-other"}}`, pub))
	if rec.Code != http.StatusForbidden {
		t.Errorf("foreign publisher_id: code=%d, want 403", rec.Code)
	}

	// Publisher with several publishers and no explicit filter: 400.
	multi := enforceReportTenant(fakePublisherLookup{ids: []string{"pub-1", "pub-2"}}, quietLog())
	rec = httptest.NewRecorder()
	multi(captureBody(t, &got)).ServeHTTP(rec, reportReq(`{"table":"auctions"}`, pub))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("multi-publisher no filter: code=%d, want 400", rec.Code)
	}

	// Staff/admin: untouched (no filters added).
	got = map[string]any{}
	staff := &auth.Claims{AccountID: "staff-1", AccountType: auth.AccountStaff}
	rec = httptest.NewRecorder()
	mw(captureBody(t, &got)).ServeHTTP(rec, reportReq(`{"table":"impressions"}`, staff))
	if rec.Code != http.StatusOK {
		t.Errorf("staff: code=%d, want 200", rec.Code)
	}
	if _, has := got["filters"]; has {
		t.Errorf("staff query should pass through unmodified, got %v", got)
	}
}
