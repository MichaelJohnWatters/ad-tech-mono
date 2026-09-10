package main

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
)

type fakeAdTagStore struct {
	p        placementTag
	gotAcc   string
	gotID    string
	notFound bool
}

func (f *fakeAdTagStore) GetPlacementForTag(_ context.Context, accountID, placementID string) (placementTag, error) {
	f.gotAcc, f.gotID = accountID, placementID
	if f.notFound {
		return placementTag{}, sql.ErrNoRows
	}
	return f.p, nil
}

func adtagReq(target string, claims *auth.Claims) *http.Request {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if claims != nil {
		req = withClaims(req, claims)
	}
	return req
}

func TestAdTagHandler(t *testing.T) {
	pub := &auth.Claims{AccountID: "acc-1", AccountType: auth.AccountPublisher,
		Permissions: []string{"placements:read"}}
	p := placementTag{ID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", Name: "MPU", Format: "display", Width: 300, Height: 250}

	// Default js tag, scoped to caller's account.
	store := &fakeAdTagStore{p: p}
	rec := httptest.NewRecorder()
	adTagHandler(store, "/sdk/v2/adtech.js", quietLog())(rec, adtagReq("/v1/api/adtag?placement_id="+p.ID, pub))
	if rec.Code != http.StatusOK || store.gotAcc != "acc-1" || store.gotID != p.ID {
		t.Fatalf("js code=%d acc=%q id=%q", rec.Code, store.gotAcc, store.gotID)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "adtech.requestAd") || !strings.Contains(body, p.ID) {
		t.Errorf("js tag missing SDK call/placement id: %s", body)
	}
	// The tag embeds the versioned SDK URL (#106), not the floating /static path.
	if !strings.Contains(body, `src=\"/sdk/v2/adtech.js\"`) {
		t.Errorf("js tag must reference the versioned SDK URL: %s", body)
	}
	if !strings.Contains(body, `"tag_type":"js"`) {
		t.Errorf("js tag_type not defaulted: %s", body)
	}

	// Prebid tag.
	rec = httptest.NewRecorder()
	adTagHandler(&fakeAdTagStore{p: p}, "/sdk/v2/adtech.js", quietLog())(rec, adtagReq("/v1/api/adtag?tag_type=prebid&placement_id="+p.ID, pub))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "pbjs.addAdUnits") {
		t.Errorf("prebid code=%d body=%s", rec.Code, rec.Body.String())
	}

	// VAST tag points at the pubad VAST route.
	rec = httptest.NewRecorder()
	adTagHandler(&fakeAdTagStore{p: placementTag{ID: p.ID, Format: "video"}}, "/sdk/v2/adtech.js", quietLog())(rec, adtagReq("/v1/api/adtag?tag_type=vast&placement_id="+p.ID, pub))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "/v1/pubad/video/vast") {
		t.Errorf("vast code=%d body=%s", rec.Code, rec.Body.String())
	}

	// Bad tag_type → 400.
	rec = httptest.NewRecorder()
	adTagHandler(&fakeAdTagStore{p: p}, "/sdk/v2/adtech.js", quietLog())(rec, adtagReq("/v1/api/adtag?tag_type=xml&placement_id="+p.ID, pub))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad tag_type code = %d, want 400", rec.Code)
	}

	// Missing placement_id → 400.
	rec = httptest.NewRecorder()
	adTagHandler(&fakeAdTagStore{p: p}, "/sdk/v2/adtech.js", quietLog())(rec, adtagReq("/v1/api/adtag", pub))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("no placement_id code = %d, want 400", rec.Code)
	}

	// Unknown/cross-tenant placement → 404.
	rec = httptest.NewRecorder()
	adTagHandler(&fakeAdTagStore{notFound: true}, "/sdk/v2/adtech.js", quietLog())(rec, adtagReq("/v1/api/adtag?placement_id=zzz", pub))
	if rec.Code != http.StatusNotFound {
		t.Errorf("not-found code = %d, want 404", rec.Code)
	}

	// Missing perm → 403.
	rec = httptest.NewRecorder()
	adTagHandler(&fakeAdTagStore{p: p}, "/sdk/v2/adtech.js", quietLog())(rec, adtagReq("/v1/api/adtag?placement_id="+p.ID, &auth.Claims{Permissions: []string{"campaigns:read"}}))
	if rec.Code != http.StatusForbidden {
		t.Errorf("no perm code = %d, want 403", rec.Code)
	}
}
