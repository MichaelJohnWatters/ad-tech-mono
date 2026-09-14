//go:build e2e

// Data residency (PLAN Phase 11 #111), control-plane slice: an account is pinned
// to a residency region; this deployment has a home region (platform.region,
// default us-east-1). An out-of-region account's MUTATIONS are rejected 403 at the
// gateway; reads are allowed; staff/admin are exempt. Staff set the region via a
// support:update-gated endpoint. Exercises the live stack end to end.
package e2e

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestDataResidencyGatesOutOfRegionMutations(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "residency")
	adv := w.AdvAcc

	setResidency := func(cl *http.Client, accID, region string) int {
		return h.APIStatus(t, cl, http.MethodPut, "/v1/api/accounts/residency",
			fmt.Sprintf(`{"account_id":%q,"residency_region":%q}`, accID, region))
	}
	const campaign = `{"name":"residency","base_bid":3.0,"daily_budget":500}`

	// Negative RBAC: the advertiser owner (no support:update) cannot set residency.
	// (Still in-region here, so this is a clean 403 from the permission gate.)
	advOwner := h.OwnerClient(t, adv.ID)
	if st := setResidency(advOwner, adv.ID, "eu"); st != http.StatusForbidden {
		t.Fatalf("advertiser set-residency: got %d, want 403 (support:update is staff-only)", st)
	}

	// Baseline: a home-region (us-east-1) advertiser can mutate.
	if st := h.APIStatus(t, advOwner, http.MethodPost, "/v1/api/campaigns", campaign); st/100 != 2 {
		t.Fatalf("home-region campaign create: got %d, want 2xx", st)
	}

	// Staff pins the advertiser to a NON-home region.
	staff := h.CreateStaff(t, fmt.Sprintf("residency-staff-%d", time.Now().UnixNano()))
	staffCl := h.OwnerClient(t, staff.ID)
	if st := setResidency(staffCl, adv.ID, "eu"); st != http.StatusOK {
		t.Fatalf("staff set-residency: got %d, want 200", st)
	}

	// The compliance-relevant residency change is audited (audit_log has no RLS →
	// bare superuser read is fine).
	var auditN int
	if err := h.DB.QueryRow(`SELECT count(*) FROM audit_log WHERE action = 'account:set_residency' AND account_id = $1::uuid`, adv.ID).Scan(&auditN); err != nil {
		t.Fatalf("audit query: %v", err)
	}
	if auditN < 1 {
		t.Errorf("staff set-residency was not audited (account:set_residency): got %d entries, want >=1", auditN)
	}

	// Fresh login → the JWT now carries residency_region=eu.
	advEU := h.OwnerClient(t, adv.ID)

	// A mutation is now rejected by the residency gate (home region is us-east-1).
	if st := h.APIStatus(t, advEU, http.MethodPost, "/v1/api/campaigns", campaign); st != http.StatusForbidden {
		t.Errorf("out-of-region campaign create: got %d, want 403 (residency gate)", st)
	}
	// Reads are NOT residency-gated — the out-of-region account can still view.
	if st := h.APIStatus(t, advEU, http.MethodGet, "/v1/api/campaigns", ""); st/100 != 2 {
		t.Errorf("out-of-region read: got %d, want 2xx (reads are not gated)", st)
	}

	// The gate is account-type-agnostic (only staff/admin exempt) — a PUBLISHER's
	// out-of-region mutation is 403'd too (proves the gate wraps publisher routes,
	// not just the advertiser campaign path).
	pub := w.PubAcc
	if st := setResidency(staffCl, pub.ID, "eu"); st != http.StatusOK {
		t.Fatalf("staff set publisher residency: got %d, want 200", st)
	}
	pubEU := h.OwnerClient(t, pub.ID)
	if st := h.APIStatus(t, pubEU, http.MethodPost, "/v1/api/publishers", `{"name":"blocked","domain":"blocked.example"}`); st != http.StatusForbidden {
		t.Errorf("out-of-region publisher mutation: got %d, want 403", st)
	}

	// Staff themselves are exempt (cross-region operators): the staff client can
	// still mutate (move the account back to the home region).
	if st := setResidency(staffCl, adv.ID, "us-east-1"); st != http.StatusOK {
		t.Errorf("staff (exempt) set-residency back: got %d, want 200", st)
	}
}
