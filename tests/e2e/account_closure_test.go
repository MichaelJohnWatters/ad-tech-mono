//go:build e2e

// Account Closure and Data Export (PLAN Phase 11, item 105) — slice 1: the
// closure request + 30-day grace state machine. Initiating a closure suspends
// the account and pauses its live campaigns / deactivates its placements (no new
// spend, no new auctions); the owner can still sign in and CANCEL during the
// grace window, which restores exactly the rows the closure touched.
package e2e

import (
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestAccountClosureGraceAndCancel(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "acct-close")
	adv := w.AdvAcc
	client := h.OwnerClient(t, adv.ID)

	// No closure to begin with.
	if c := h.AccountCloseStatus(t, client); c != nil {
		t.Fatalf("expected no active closure, got %+v", c)
	}

	// The campaign is live before closure.
	if got := lineItemStatus(t, h, w.Campaign.ID); got != "live" {
		t.Fatalf("precondition: campaign status = %q, want live", got)
	}

	// Initiate closure → grace, account suspended, campaign paused.
	st, cr := h.AccountClose(t, client)
	if st != 200 {
		t.Fatalf("close initiate status %d", st)
	}
	if cr.Status != "grace" {
		t.Errorf("closure status = %q, want grace", cr.Status)
	}
	if cr.GraceEndsAt.Before(time.Now().Add(29 * 24 * time.Hour)) {
		t.Errorf("grace_ends_at = %v, want ~30 days out", cr.GraceEndsAt)
	}
	if len(cr.PausedLineItems) == 0 {
		t.Errorf("expected the live campaign to be captured in paused_line_items")
	}
	if got := accountStatus(t, h, adv.ID); got != "suspended" {
		t.Errorf("account status = %q, want suspended", got)
	}
	if got := lineItemStatus(t, h, w.Campaign.ID); got != "paused" {
		t.Errorf("campaign status after close = %q, want paused", got)
	}

	// GET reflects the grace closure.
	if c := h.AccountCloseStatus(t, client); c == nil || c.Status != "grace" {
		t.Errorf("status endpoint did not reflect the grace closure: %+v", c)
	}

	// A second initiate conflicts (one grace closure at a time).
	if st2, _ := h.AccountClose(t, client); st2 != 409 {
		t.Errorf("second close initiate status = %d, want 409", st2)
	}

	// Cancel → account active again, campaign restored to live, request cancelled.
	stc, crc := h.AccountCloseCancel(t, client)
	if stc != 200 {
		t.Fatalf("cancel status %d", stc)
	}
	if crc.Status != "cancelled" {
		t.Errorf("cancelled closure status = %q, want cancelled", crc.Status)
	}
	if got := accountStatus(t, h, adv.ID); got != "active" {
		t.Errorf("account status after cancel = %q, want active", got)
	}
	if got := lineItemStatus(t, h, w.Campaign.ID); got != "live" {
		t.Errorf("campaign status after cancel = %q, want live (exact reversal)", got)
	}
	if c := h.AccountCloseStatus(t, client); c != nil {
		t.Errorf("expected no active closure after cancel, got %+v", c)
	}

	// Cancel again with nothing open → 404.
	if st3, _ := h.AccountCloseCancel(t, client); st3 != 404 {
		t.Errorf("cancel with no closure = %d, want 404", st3)
	}
}

// TestAccountClosureDeactivatesPlacements closes the PUBLISHER account and
// verifies the publisher-side side effect: active placements go inactive, then
// restore exactly on cancel.
func TestAccountClosureDeactivatesPlacements(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "acct-close-pub")
	pub := w.PubAcc
	client := h.OwnerClient(t, pub.ID)

	if got := placementStatus(t, h, w.Placement.ID); got != "active" {
		t.Fatalf("precondition: placement status = %q, want active", got)
	}

	st, cr := h.AccountClose(t, client)
	if st != 200 {
		t.Fatalf("close initiate status %d", st)
	}
	if len(cr.DeactivatedPlacements) == 0 {
		t.Errorf("expected the active placement captured in deactivated_placements")
	}
	if got := placementStatus(t, h, w.Placement.ID); got != "inactive" {
		t.Errorf("placement status after close = %q, want inactive", got)
	}

	if _, crc := h.AccountCloseCancel(t, client); crc.Status != "cancelled" {
		t.Errorf("cancel status = %q, want cancelled", crc.Status)
	}
	if got := placementStatus(t, h, w.Placement.ID); got != "active" {
		t.Errorf("placement status after cancel = %q, want active (exact reversal)", got)
	}
}

// lineItemStatus lives in dayboundary_test.go (same package) — reused here.

func placementStatus(t *testing.T, h *harness.Harness, id string) string {
	t.Helper()
	var s string
	if err := h.DB.QueryRow(`SELECT status FROM placements WHERE id = $1::uuid`, id).Scan(&s); err != nil {
		t.Fatalf("placement status: %v", err)
	}
	return s
}

func accountStatus(t *testing.T, h *harness.Harness, id string) string {
	t.Helper()
	var s string
	if err := h.DB.QueryRow(`SELECT status FROM accounts WHERE id = $1::uuid`, id).Scan(&s); err != nil {
		t.Fatalf("account status: %v", err)
	}
	return s
}
