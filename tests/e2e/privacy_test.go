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
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
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
	t.Skip("The SSP now stamps Regs/consent onto the outbound request and RunAuctionWith carries the privacy params (GDPR/Consent/USPrivacy/COPPA/GPP), so the Regs path reaches the DSP. Still blocked on observing WHICH segments the DSP used: asserting 'bid won but WITHOUT behavioural targeting' needs the auction response (or a DSP debug echo) to report the segments applied. Registry opt-out → no-bid is covered by TestPrivacyOptOutBlocksServe; the SSP signal-population logic is unit-tested in cmd/ssp (TestApplyPrivacySignals).")
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

	// Lake systems: a profile signal (audience upload) + a behaviour row (a
	// consented ad request) — both land async via NATS → pipeline flush.
	h.UploadAudience(t, w.AdvAcc.ID, "e2e-privacy-lake-"+userID, "public", []string{userID})
	h.RunAuctionWith(t, harness.AuctionParams{
		Placement: w.Placement.ExternalID, Geo: "GBR", Device: "mobile", UserID: userID,
	})

	// freq_cap_blocks (ClickHouse) — the operational-signal table that
	// carries user_id; the purge gap this phase closes.
	ch, err := analytics.NewClickHouse(analytics.ClickHouseConfig{
		Addrs: []string{routes.DefaultClickHouseNativeAddr}, Database: "adtech",
		Username: "adtech", Password: "adtech-local-dev", Log: quiet,
	})
	if err != nil {
		t.Fatalf("clickhouse connect (is the stack up?): %v", err)
	}
	ch.InsertFreqCapBlock(analytics.FreqCapBlock{
		TraceID: "trace-" + userID, UserID: userID, CampaignID: w.Campaign.ID,
		PlacementID: w.Placement.ID, PublisherID: w.Publisher.ID, Timestamp: time.Now().UTC(),
	})

	h.SetOptOut(t, userID, 3) // level 3 = full deletion

	// Precondition: the data is actually there — including the async signal
	// rows in ClickHouse (NATS → reporting → ClickHouse), which must have LANDED
	// before the purge runs or the delete no-ops and the rows arrive afterwards
	// as residuals. (Since ADR 0006 phase 5 the profile-store signals live in
	// ClickHouse, not the retired Delta lake.)
	if h.IdentityEdgeCount(t, userID) == 0 {
		t.Fatal("precondition: expected a seeded identity edge")
	}
	if h.UserSegmentMembershipCount(t, userID) == 0 {
		t.Fatal("precondition: expected a seeded segment membership")
	}
	signalDeadline := time.Now().Add(60 * time.Second)
	for {
		counts := h.SignalResidual(t, userID)
		if counts["profile_signals"] > 0 && counts["behaviour_signals"] > 0 {
			break
		}
		if time.Now().After(signalDeadline) {
			t.Fatalf("precondition: clickhouse signal rows never landed (counts=%v)", counts)
		}
		time.Sleep(2 * time.Second)
	}
	if n, err := ch.CountFreqCapBlocks(context.Background(), userID); err != nil || n == 0 {
		t.Fatalf("precondition: freq_cap_blocks row missing (n=%d err=%v)", n, err)
	}

	// Run the deletion pipeline against the live DB with the full extra set,
	// exactly as the cmd/privacy-delete CronJob wires it (BuildExtras).
	extras := []privacydelete.ExtraPurger{
		&privacydelete.SignalsPurger{Store: ch},
		&privacydelete.FreqCapPurger{Store: ch},
	}
	deleter := &privacydelete.Deleter{
		Store: privacydelete.NewPostgresStore(h.DB).WithExtras(extras...),
		Log:   quiet,
	}
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
	for table, n := range h.SignalResidual(t, userID) {
		if n != 0 {
			t.Errorf("clickhouse table %s not purged: %d rows remain", table, n)
		}
	}
	if n, _ := ch.CountFreqCapBlocks(context.Background(), userID); n != 0 {
		t.Errorf("freq_cap_blocks not purged: %d rows remain", n)
	}

	// Verifier: no residual data (Postgres + lake + freq caps) → verified_at.
	verifier := &privacydelete.Verifier{
		Store: privacydelete.NewPostgresStore(h.DB).WithExtras(extras...),
		Log:   quiet,
	}
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
