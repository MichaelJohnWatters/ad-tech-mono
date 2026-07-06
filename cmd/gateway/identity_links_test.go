package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/identity"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

type fakeIDStore struct {
	got []postgres.IdentityEdge
	err error
}

func (f *fakeIDStore) LinkIdentity(_ context.Context, edges []postgres.IdentityEdge) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.got = append(f.got, edges...)
	return len(edges), nil
}

func postJSON(t *testing.T, h http.HandlerFunc, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("POST", "/v1/api/identity-links", strings.NewReader(body)))
	return rec
}

func TestIdentityLinks_UID2Shape(t *testing.T) {
	st := &fakeIDStore{}
	h := identityLinksHandler(st, quietLog())

	rec := postJSON(t, h, `{"uid2":"tok-1","links":[{"id":"email-hash-1"},{"id":"dev-1","source":"device_id","link_type":"cross_device"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp identityLinkResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.LinksWritten != 2 {
		t.Errorf("links_written = %d, want 2", resp.LinksWritten)
	}
	if len(st.got) != 2 {
		t.Fatalf("store got %d edges, want 2", len(st.got))
	}
	// First link defaults to source=uid2, link_type=cross_device.
	e0 := st.got[0]
	if e0.UserID != "tok-1" || e0.LinkedID != "email-hash-1" || e0.Source != identity.SourceUID2 || e0.LinkType != identity.LinkCrossDevice {
		t.Errorf("edge0 = %+v, want uid2-defaulted edge", e0)
	}
	// Second link honours explicit source/link_type.
	if st.got[1].Source != "device_id" {
		t.Errorf("edge1 source = %q, want device_id", st.got[1].Source)
	}
}

func TestIdentityLinks_GenericEdges(t *testing.T) {
	st := &fakeIDStore{}
	h := identityLinksHandler(st, quietLog())
	rec := postJSON(t, h, `{"edges":[{"user_id":"a","linked_id":"b","confidence":0.7}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if len(st.got) != 1 || st.got[0].UserID != "a" || st.got[0].LinkedID != "b" {
		t.Fatalf("unexpected edges: %+v", st.got)
	}
	if st.got[0].Source != identity.SourceHashedEmail {
		t.Errorf("generic edge default source = %q, want hashed_email", st.got[0].Source)
	}
}

func TestIdentityLinks_Rejects(t *testing.T) {
	st := &fakeIDStore{}
	h := identityLinksHandler(st, quietLog())

	// No usable edges (uid2 without links, no edges) → 400.
	if rec := postJSON(t, h, `{"uid2":"tok-1"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("empty payload: status %d, want 400", rec.Code)
	}
	// Malformed JSON → 400.
	if rec := postJSON(t, h, `not json`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad json: status %d, want 400", rec.Code)
	}
	// Nil store → 503.
	nilH := identityLinksHandler(nil, quietLog())
	if rec := postJSON(t, nilH, `{"uid2":"x","links":[{"id":"y"}]}`); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("nil store: status %d, want 503", rec.Code)
	}
}
