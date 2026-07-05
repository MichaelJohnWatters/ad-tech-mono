//go:build e2e

// Privacy tests — opt-out and consent flow.
package e2e

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/privacydelete"
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

// TestPrivacyDeletionPropagation — a Level-3 (full deletion) request must purge
// the user's data across systems. Seed an identity-graph edge + a segment
// membership + a level-3 opt_out_registry row, run the deletion pipeline (the
// same pkg the cmd/privacy-delete job runs), then assert every system is empty
// for the user and the registry row is marked completed. The verifier then
// confirms no residual data and stamps verified_at.
func TestPrivacyDeletionPropagation(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "privacy-del")
	userID := fmt.Sprintf("privacy-del-user-%d", time.Now().UnixNano())
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Seed cross-system data for the user.
	h.AddIdentityEdge(t, userID, "device-"+userID, "cross_device")
	seg := h.CreateSegment(t, w.AdvAcc, "e2e-seg-privacy-del")
	h.AddUserToSegment(t, seg, userID)
	h.SetOptOut(t, userID, 3) // level 3 = full deletion

	// Precondition: the data is actually there.
	if h.IdentityEdgeCount(t, userID) == 0 {
		t.Fatal("precondition: expected a seeded identity edge")
	}
	if h.UserSegmentMembershipCount(t, userID) == 0 {
		t.Fatal("precondition: expected a seeded segment membership")
	}

	// Run the deletion pipeline against the live DB, exactly as the
	// cmd/privacy-delete CronJob does.
	deleter := &privacydelete.Deleter{Store: privacydelete.NewPostgresStore(h.DB), Log: quiet}
	n, err := deleter.RunPending(context.Background())
	if err != nil {
		t.Fatalf("RunPending: %v", err)
	}
	if n < 1 {
		t.Fatalf("expected at least 1 deletion processed, got %d", n)
	}

	// Propagation: every system purged, registry row completed.
	if got := h.IdentityEdgeCount(t, userID); got != 0 {
		t.Errorf("identity_graph not purged: %d rows remain", got)
	}
	if got := h.UserSegmentMembershipCount(t, userID); got != 0 {
		t.Errorf("audience_segment_members not purged: %d rows remain", got)
	}
	if !h.OptOutCompleted(t, userID) {
		t.Error("opt_out_registry.completed_at not set after deletion")
	}

	// Verifier: no residual data → stamps verified_at.
	verifier := &privacydelete.Verifier{Store: privacydelete.NewPostgresStore(h.DB), Log: quiet}
	verified, incomplete, err := verifier.RunUnverified(context.Background())
	if err != nil {
		t.Fatalf("RunUnverified: %v", err)
	}
	if incomplete != 0 {
		t.Errorf("verifier reported %d incomplete deletions (residual data)", incomplete)
	}
	if verified < 1 {
		t.Errorf("expected at least 1 verified deletion, got %d", verified)
	}
	if !h.OptOutVerified(t, userID) {
		t.Error("opt_out_registry.verified_at not set after verification")
	}

	h.ClearOptOut(t, userID)
}
