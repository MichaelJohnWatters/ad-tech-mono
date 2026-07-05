package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
)

type fakeRevshareStore struct {
	list     []revshareView
	gotID    string
	gotActor string
	gotPatch revsharePatch
}

func (f *fakeRevshareStore) ListRevshare(context.Context) ([]revshareView, error) {
	return f.list, nil
}
func (f *fakeRevshareStore) UpdateRevshare(_ context.Context, id, actor string, in revsharePatch) error {
	f.gotID, f.gotActor, f.gotPatch = id, actor, in
	return nil
}

func rsReq(method, url, body string, claims *auth.Claims) *http.Request {
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	if claims != nil {
		req = withClaims(req, claims)
	}
	return req
}

func TestRevshareHandler(t *testing.T) {
	staff := &auth.Claims{UserID: "u1", AccountType: auth.AccountStaff, Permissions: []string{"support:read", "support:update"}}
	pubID := "11111111-1111-4111-8111-111111111111"

	// GET list.
	store := &fakeRevshareStore{list: []revshareView{{PublisherID: pubID, Name: "Daily News", FeePct: 20}}}
	rec := httptest.NewRecorder()
	revshareHandler(store, nil, quietLog())(rec, rsReq(http.MethodGet, "/v1/api/revshare", "", staff))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Daily News") {
		t.Fatalf("list code=%d body=%s", rec.Code, rec.Body.String())
	}

	// PATCH valid → parsed + publishes billing-rates invalidate.
	store = &fakeRevshareStore{}
	bus := &countingBus{}
	rec = httptest.NewRecorder()
	revshareHandler(store, bus, quietLog())(rec, rsReq(http.MethodPatch, "/v1/api/revshare?id="+pubID, `{"revshare_model":"fixed","fee_pct":15}`, staff))
	if rec.Code != http.StatusOK || store.gotID != pubID || store.gotActor != "u1" || store.gotPatch.FeePct != 15 {
		t.Fatalf("patch code=%d id=%q actor=%q patch=%+v", rec.Code, store.gotID, store.gotActor, store.gotPatch)
	}
	if bus.published != 1 || bus.subject != events.SubjectCacheInvalidateBillingRates {
		t.Errorf("patch invalidate: published=%d subject=%q", bus.published, bus.subject)
	}

	// PATCH a tiered config + payment terms → parsed through to the store.
	store = &fakeRevshareStore{}
	rec = httptest.NewRecorder()
	revshareHandler(store, &countingBus{}, quietLog())(rec, rsReq(http.MethodPatch, "/v1/api/revshare?id="+pubID,
		`{"revshare_model":"tiered","payment_terms":"net_60","tiers":[{"min_impressions":0,"max_impressions":1000000,"fee_pct":25},{"min_impressions":1000000,"max_impressions":0,"fee_pct":18}]}`, staff))
	if rec.Code != http.StatusOK || len(store.gotPatch.Tiers) != 2 || store.gotPatch.PaymentTerms != "net_60" {
		t.Fatalf("tiered patch code=%d patch=%+v", rec.Code, store.gotPatch)
	}

	// Bad fee / bad model / bad id / non-contiguous tiers / bad tier fee / bad
	// payment_terms → 400.
	for _, tc := range []struct{ url, body string }{
		{"/v1/api/revshare?id=" + pubID, `{"fee_pct":150}`},
		{"/v1/api/revshare?id=" + pubID, `{"revshare_model":"bogus","fee_pct":10}`},
		{"/v1/api/revshare?id=not-a-uuid", `{"fee_pct":10}`},
		{"/v1/api/revshare?id=" + pubID, `{"revshare_model":"tiered","tiers":[{"min_impressions":0,"max_impressions":1000,"fee_pct":25},{"min_impressions":2000,"max_impressions":0,"fee_pct":18}]}`},
		{"/v1/api/revshare?id=" + pubID, `{"revshare_model":"tiered","tiers":[{"min_impressions":0,"max_impressions":0,"fee_pct":250}]}`},
		{"/v1/api/revshare?id=" + pubID, `{"payment_terms":"net_45"}`},
		{"/v1/api/revshare?id=" + pubID, `{"guaranteed_min_cpm":-1}`},
	} {
		rec = httptest.NewRecorder()
		revshareHandler(&fakeRevshareStore{}, nil, quietLog())(rec, rsReq(http.MethodPatch, tc.url, tc.body, staff))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s %s: code=%d, want 400", tc.url, tc.body, rec.Code)
		}
	}

	// Non-staff (advertiser) → 403 on both read and write.
	adv := &auth.Claims{AccountType: auth.AccountAdvertiser, Permissions: []string{"reports:read"}}
	rec = httptest.NewRecorder()
	revshareHandler(&fakeRevshareStore{}, nil, quietLog())(rec, rsReq(http.MethodGet, "/v1/api/revshare", "", adv))
	if rec.Code != http.StatusForbidden {
		t.Errorf("advertiser GET code=%d, want 403", rec.Code)
	}
}
