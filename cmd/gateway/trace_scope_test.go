package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
)

// captureScope records the X-Publisher-ID the middleware forwarded and 200s.
func captureScope(got *string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = r.Header.Get(constants.HeaderPublisherID)
		w.WriteHeader(http.StatusOK)
	})
}

func traceScopeReq(url string, claims *auth.Claims) *http.Request {
	req := httptest.NewRequest(http.MethodGet, url, nil)
	if claims != nil {
		req = withClaims(req, claims)
	}
	return req
}

func TestInjectTraceScope_PublisherOwned(t *testing.T) {
	var fwd string
	mw := injectTraceScope(fakePublisherLookup{ids: []string{"pub-1", "pub-2"}}, quietLog())
	req := traceScopeReq("/v1/api/trace?trace_id=T&publisher_id=pub-2", &auth.Claims{AccountType: auth.AccountPublisher, AccountID: "acct-1"})
	rec := httptest.NewRecorder()
	mw(captureScope(&fwd)).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d, want 200", rec.Code)
	}
	if fwd != "pub-2" {
		t.Fatalf("forwarded X-Publisher-ID=%q, want pub-2", fwd)
	}
}

func TestInjectTraceScope_PublisherNotOwned403(t *testing.T) {
	mw := injectTraceScope(fakePublisherLookup{ids: []string{"pub-1"}}, quietLog())
	req := traceScopeReq("/v1/api/trace?publisher_id=pub-999", &auth.Claims{AccountType: auth.AccountPublisher, AccountID: "acct-1"})
	rec := httptest.NewRecorder()
	mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d, want 403 (publisher not in account)", rec.Code)
	}
}

func TestInjectTraceScope_SinglePublisherAutoSelected(t *testing.T) {
	var fwd string
	mw := injectTraceScope(fakePublisherLookup{ids: []string{"pub-only"}}, quietLog())
	req := traceScopeReq("/v1/api/impressions/recent", &auth.Claims{AccountType: auth.AccountPublisher, AccountID: "acct-1"})
	rec := httptest.NewRecorder()
	mw(captureScope(&fwd)).ServeHTTP(rec, req)
	if fwd != "pub-only" {
		t.Fatalf("auto-select failed: X-Publisher-ID=%q, want pub-only", fwd)
	}
}

// A client that forges X-Publisher-ID must not have it survive — the middleware
// strips it before any downstream sees it (advertiser sessions never carry one).
func TestInjectTraceScope_ForgedHeaderStripped(t *testing.T) {
	fwd := "sentinel"
	mw := injectTraceScope(fakePublisherLookup{}, quietLog())
	req := traceScopeReq("/v1/api/trace?trace_id=T", &auth.Claims{AccountType: auth.AccountAdvertiser, AccountID: "acct-adv"})
	req.Header.Set(constants.HeaderPublisherID, "pub-forged")
	rec := httptest.NewRecorder()
	mw(captureScope(&fwd)).ServeHTTP(rec, req)
	if fwd != "" {
		t.Fatalf("forged X-Publisher-ID survived: %q", fwd)
	}
}
