//go:build e2e

// Privacy tests — opt-out and consent flow.
package e2e

import (
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// TestPrivacyOptOutBlocksServe — a user in opt_out_registry at level 2
// (no tracking) must get no bid from any DSP. Baseline: the same user
// with no opt-out wins; after the opt-out row + cache refresh, the DSP
// short-circuits to NoBid before targeting.
func TestPrivacyOptOutBlocksServe(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "privacy-optout")
	userID := "privacy-optout-user"

	// Baseline: consented user wins.
	h.ClearOptOut(t, userID)
	h.RefreshAllCaches(t)
	win := h.ExtractWinner(t, h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", userID))
	if win.NoBid {
		t.Fatalf("baseline: expected a winning bid for a consented user, got NoBid")
	}

	// Opt out of tracking (level 2) → DSP must not bid.
	h.SetOptOut(t, userID, 2)
	h.RefreshAllCaches(t)
	win = h.ExtractWinner(t, h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", userID))
	if !win.NoBid {
		t.Errorf("expected NoBid for opted-out (level 2) user; got winner %q at %.4f", win.Seat, win.Price)
	}

	// A different, non-opted-out user still wins (the block is per-user).
	other := "privacy-consented-user"
	h.ClearOptOut(t, other)
	h.RefreshAllCaches(t)
	win = h.ExtractWinner(t, h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", other))
	if win.NoBid {
		t.Errorf("expected a bid for a non-opted-out user; got NoBid")
	}

	// Cleanup the global rows so they don't leak into later tests.
	h.ClearOptOut(t, userID)
	h.RefreshAllCaches(t)
}

func TestPrivacyConsentSignalPropagated(t *testing.T) {
	t.Skip("DSP now enforces OpenRTB regs (GDPR-no-consent / COPPA / US-privacy → contextual-only) and the opt-out registry, but asserting 'bid won WITHOUT behavioural targeting' needs a RunAuction variant that sets Regs + a way to observe which segments were used; pending that harness support. Registry opt-out → no-bid is covered by TestPrivacyOptOutBlocksServe.")
}

func TestPrivacyPIINotInLogs(t *testing.T) {
	t.Skip("would require sweeping stdout/stderr of every service for PII patterns; out of scope for this functional pass — covered by code review + linters")
}

func TestPrivacyDeletionPropagation(t *testing.T) {
	t.Skip("user-deletion flow needs cmd/privacy-delete built (currently an empty cmd/ shell); test ready when service is up")
}
