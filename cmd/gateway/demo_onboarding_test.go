package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// The permission gating is enforced before the store is consulted, so these
// checks run without Postgres. The store-backed behaviour (real reset + real
// profile-builder expansion) is covered in demo_onboarding_integration_test.go.

func TestDemoOnboardingHandler_Permissions(t *testing.T) {
	// nil-db orchestrator: permission checks must still short-circuit BEFORE the
	// 503 store-unavailable path, so a 403 never leaks store wiring.
	o := &demoOrchestrator{db: nil, log: quietLog()}
	h := demoOnboardingHandler(o)

	req := func(method, target string, claims *auth.Claims) *http.Request {
		r := httptest.NewRequest(method, target, nil)
		if claims != nil {
			r = withClaims(r, claims)
		}
		return r
	}

	staffRead := &auth.Claims{AccountType: auth.AccountStaff, Permissions: []string{"support:read"}}
	staffUpdate := &auth.Claims{AccountType: auth.AccountStaff, Permissions: []string{"support:read", "support:update"}}
	advertiser := &auth.Claims{AccountType: auth.AccountAdvertiser, Permissions: []string{"reports:read"}}

	cases := []struct {
		name   string
		method string
		target string
		claims *auth.Claims
		want   int
	}{
		{"no claims → 401", http.MethodGet, routes.APIDemoOnboarding, nil, http.StatusUnauthorized},
		{"advertiser view → 403", http.MethodGet, routes.APIDemoOnboarding, advertiser, http.StatusForbidden},
		{"advertiser run → 403", http.MethodPost, routes.APIDemoOnboardingRun, advertiser, http.StatusForbidden},
		// support:read may VIEW but NOT run (support:update required).
		{"read-only staff run → 403", http.MethodPost, routes.APIDemoOnboardingRun, staffRead, http.StatusForbidden},
		// With the right permission, gating passes and we reach the nil-store 503.
		{"staff view reaches store → 503", http.MethodGet, routes.APIDemoOnboarding, staffRead, http.StatusServiceUnavailable},
		{"staff run reaches store → 503", http.MethodPost, routes.APIDemoOnboardingRun, staffUpdate, http.StatusServiceUnavailable},
		{"unsupported method → 405", http.MethodDelete, routes.APIDemoOnboarding, staffUpdate, http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h(rec, req(tc.method, tc.target, tc.claims))
			if rec.Code != tc.want {
				t.Fatalf("code = %d, want %d (body=%s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// TestBuildProfileView_NilDB documents the nil-db contract buildProfileView
// relies on (the demo passes a real *sql.DB in production, a nil resolver is
// tolerated): with no DB it returns the identity-only singleton view.
func TestBuildProfileView_NilDB(t *testing.T) {
	view, err := buildProfileView(context.Background(), nil, nil, demoEmailID)
	if err != nil {
		t.Fatalf("buildProfileView(nil db) err = %v", err)
	}
	if view.ID != demoEmailID {
		t.Errorf("ID = %q, want %q", view.ID, demoEmailID)
	}
	if len(view.ClusterMembers) != 1 || view.ClusterMembers[0] != demoEmailID {
		t.Errorf("ClusterMembers = %v, want singleton [%s]", view.ClusterMembers, demoEmailID)
	}
	if len(view.Memberships) != 0 {
		t.Errorf("Memberships = %v, want empty", view.Memberships)
	}
}

// TestDemoDiff exercises the BEFORE→AFTER membership diff helper: AFTER must be
// a superset of BEFORE and the diff is exactly what expansion added.
func TestDemoDiff(t *testing.T) {
	before := []string{demoEmailID}
	after := []string{demoCookieID, demoEmailID, demoIfaID}
	got := demoDiff(before, after)
	want := []string{demoCookieID, demoIfaID}
	if len(got) != len(want) {
		t.Fatalf("diff = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("diff = %v, want %v", got, want)
		}
	}
	// The household id must never appear in a segment diff.
	for _, id := range after {
		if id == demoHouseholdID {
			t.Fatalf("household id leaked into AFTER membership: %v", after)
		}
	}
}
