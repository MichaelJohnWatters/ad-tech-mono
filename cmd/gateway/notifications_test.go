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
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/notifications"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// fakeNotifStore is an in-memory notifications.Store that scopes every method by
// account, so the handler tests can assert tenant isolation without a database.
type fakeNotifStore struct {
	rows map[string][]notifications.Notification // account_id -> notifications
}

func (f *fakeNotifStore) Insert(_ context.Context, n notifications.Notification) error {
	if f.rows == nil {
		f.rows = map[string][]notifications.Notification{}
	}
	f.rows[n.AccountID] = append(f.rows[n.AccountID], n)
	return nil
}

func (f *fakeNotifStore) ListForAccount(_ context.Context, accountID string, limit int) ([]notifications.Notification, error) {
	out := append([]notifications.Notification(nil), f.rows[accountID]...)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeNotifStore) UnreadCount(_ context.Context, accountID string) (int, error) {
	n := 0
	for _, r := range f.rows[accountID] {
		if !r.Read {
			n++
		}
	}
	return n, nil
}

func (f *fakeNotifStore) MarkRead(_ context.Context, accountID, id string) error {
	for i := range f.rows[accountID] {
		if f.rows[accountID][i].ID == id {
			f.rows[accountID][i].Read = true
			return nil
		}
	}
	return sql.ErrNoRows
}

func (f *fakeNotifStore) MarkAllRead(_ context.Context, accountID string) error {
	for i := range f.rows[accountID] {
		f.rows[accountID][i].Read = true
	}
	return nil
}

const (
	notifAcct  = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	notifOther = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
)

func notifClaims(accountID string) *auth.Claims {
	return &auth.Claims{AccountID: accountID, AccountType: auth.AccountAdvertiser}
}

func TestNotificationsHandler_ListTenantScopedWithUnread(t *testing.T) {
	store := &fakeNotifStore{rows: map[string][]notifications.Notification{
		notifAcct: {
			{ID: "n1", AccountID: notifAcct, Kind: notifications.KindBudgetDepleted, Title: "Budget", Read: false},
			{ID: "n2", AccountID: notifAcct, Kind: notifications.KindCampaignState, Title: "Paused", Read: true},
		},
		notifOther: {
			{ID: "n3", AccountID: notifOther, Kind: notifications.KindBalanceDepleted, Title: "Balance", Read: false},
		},
	}}
	rec := httptest.NewRecorder()
	req := withClaims(httptest.NewRequest(http.MethodGet, routes.APINotifications, nil), notifClaims(notifAcct))
	notificationsHandler(store, quietLog())(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET code = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var resp notificationsListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Notifications) != 2 {
		t.Fatalf("got %d notifications, want the caller's 2 only: %+v", len(resp.Notifications), resp.Notifications)
	}
	if resp.Unread != 1 {
		t.Errorf("unread = %d, want 1", resp.Unread)
	}
	// The other tenant's notification must never appear.
	for _, n := range resp.Notifications {
		if n.AccountID != notifAcct {
			t.Errorf("leaked cross-tenant notification: %+v", n)
		}
	}
}

func TestNotificationsHandler_MarkReadOne(t *testing.T) {
	store := &fakeNotifStore{rows: map[string][]notifications.Notification{
		notifAcct: {{ID: "n1", AccountID: notifAcct, Title: "Budget", Read: false}},
	}}
	rec := httptest.NewRecorder()
	req := withClaims(httptest.NewRequest(http.MethodPost, routes.APINotifications+"/read",
		strings.NewReader(`{"id":"n1"}`)), notifClaims(notifAcct))
	notificationsHandler(store, quietLog())(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST read code = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !store.rows[notifAcct][0].Read {
		t.Error("notification was not marked read")
	}
}

func TestNotificationsHandler_MarkReadCrossTenantIsolated(t *testing.T) {
	// notifOther owns n3; a mark-read from notifAcct must not touch it (the fake
	// scopes by account, returning ErrNoRows → 404).
	store := &fakeNotifStore{rows: map[string][]notifications.Notification{
		notifOther: {{ID: "n3", AccountID: notifOther, Title: "Balance", Read: false}},
	}}
	rec := httptest.NewRecorder()
	req := withClaims(httptest.NewRequest(http.MethodPost, routes.APINotifications+"/read",
		strings.NewReader(`{"id":"n3"}`)), notifClaims(notifAcct))
	notificationsHandler(store, quietLog())(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant mark-read code = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	if store.rows[notifOther][0].Read {
		t.Error("other tenant's notification was marked read")
	}
}

func TestNotificationsHandler_MarkAllRead(t *testing.T) {
	store := &fakeNotifStore{rows: map[string][]notifications.Notification{
		notifAcct: {
			{ID: "n1", AccountID: notifAcct, Read: false},
			{ID: "n2", AccountID: notifAcct, Read: false},
		},
	}}
	rec := httptest.NewRecorder()
	req := withClaims(httptest.NewRequest(http.MethodPost, routes.APINotifications+"/read",
		strings.NewReader(`{"all":true}`)), notifClaims(notifAcct))
	notificationsHandler(store, quietLog())(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("mark-all code = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	for _, n := range store.rows[notifAcct] {
		if !n.Read {
			t.Errorf("notification %s not marked read", n.ID)
		}
	}
}

func TestNotificationsHandler_NoClaimsUnauthorized(t *testing.T) {
	rec := httptest.NewRecorder()
	notificationsHandler(&fakeNotifStore{}, quietLog())(rec,
		httptest.NewRequest(http.MethodGet, routes.APINotifications, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no-claims code = %d, want 401", rec.Code)
	}
}
