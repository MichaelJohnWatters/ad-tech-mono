package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
)

type fakeCreativeStore struct {
	gotAccount string
	gotInput   creativeInput
}

func (f *fakeCreativeStore) CreateCreative(_ context.Context, accountID string, in creativeInput) (string, error) {
	f.gotAccount = accountID
	f.gotInput = in
	return "cr-new", nil
}

func (f *fakeCreativeStore) ListCreatives(_ context.Context, accountID string) ([]creativeView, error) {
	f.gotAccount = accountID
	return []creativeView{{ID: "cr-1", Name: "banner", ReviewStatus: "approved"}}, nil
}

func postCreative(h http.HandlerFunc, body string, claims *auth.Claims) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/api/creatives", strings.NewReader(body))
	if claims != nil {
		req = withClaims(req, claims)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func TestCreativeUpload(t *testing.T) {
	store := &fakeCreativeStore{}
	h := creativeUploadHandler(store, quietLog())
	adv := &auth.Claims{AccountID: "aaaaaaa9-9999-4999-8999-999999999999", Permissions: []string{"creatives:upload"}}

	// Valid → 201, tenant-scoped, pending_review.
	rec := postCreative(h, `{"name":"banner","format":"display","width":300,"height":250,"landing_url":"https://x.test"}`, adv)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload code = %d, want 201; body %s", rec.Code, rec.Body.String())
	}
	if store.gotAccount != "aaaaaaa9-9999-4999-8999-999999999999" || store.gotInput.Name != "banner" {
		t.Errorf("not scoped/parsed: account=%q input=%+v", store.gotAccount, store.gotInput)
	}
	if !strings.Contains(rec.Body.String(), "pending_review") {
		t.Errorf("response should report pending_review")
	}

	// Missing landing_url → 400.
	if rec := postCreative(h, `{"name":"x"}`, adv); rec.Code != http.StatusBadRequest {
		t.Errorf("missing landing_url code = %d, want 400", rec.Code)
	}
	// Bad format → 400.
	if rec := postCreative(h, `{"name":"x","format":"hologram","landing_url":"https://x"}`, adv); rec.Code != http.StatusBadRequest {
		t.Errorf("bad format code = %d, want 400", rec.Code)
	}
	// No perm → 403.
	if rec := postCreative(h, `{"name":"x","landing_url":"https://x"}`, &auth.Claims{AccountID: "aaaaaaa9-9999-4999-8999-999999999999", Permissions: []string{"campaigns:read"}}); rec.Code != http.StatusForbidden {
		t.Errorf("no perm code = %d, want 403", rec.Code)
	}
	// No claims → 401.
	if rec := postCreative(h, `{"name":"x","landing_url":"https://x"}`, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("no claims code = %d, want 401", rec.Code)
	}
}
