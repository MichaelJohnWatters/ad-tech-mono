package privacy

import (
	"context"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/identity"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
)

func setupManager() (*Manager, *identity.Graph, *audience.Store) {
	graph := identity.NewGraph()
	audiences := audience.NewStore()
	log := logger.New("privacy-test")
	mgr := NewManager(graph, audiences, log)
	return mgr, graph, audiences
}

func TestCheckConsent_Default(t *testing.T) {
	mgr, _, _ := setupManager()
	ctx := context.Background()

	// No opt-out = full consent
	if !mgr.CheckConsent(ctx, "user-1", PurposeTargeting) {
		t.Error("expected consent by default")
	}
}

func TestOptOut_Level1(t *testing.T) {
	mgr, graph, audiences := setupManager()
	ctx := context.Background()

	// Setup user data
	graph.Link("user-1", identity.Signal{Type: "email", Value: "hash123"})
	audiences.CreateSegment(ctx, audience.Segment{ID: "seg-1", AccountID: "a"})
	audiences.AddUsers(ctx, "seg-1", []string{"user-1"})

	// Level 1: no personalisation
	result := mgr.OptOut(ctx, "user-1", LevelNoPersonalisation, "cmp")

	if result.SegmentsRemoved != 1 {
		t.Errorf("segments_removed = %d, want 1", result.SegmentsRemoved)
	}

	// Targeting not allowed, measurement still is
	if mgr.CheckConsent(ctx, "user-1", PurposeTargeting) {
		t.Error("targeting should be blocked at Level 1")
	}
	if !mgr.CheckConsent(ctx, "user-1", PurposeMeasurement) {
		t.Error("measurement should still be allowed at Level 1")
	}

	// Identity graph should still exist
	profile := graph.Profile("user-1")
	if profile == nil {
		t.Error("identity graph should survive Level 1")
	}
}

func TestOptOut_Level2(t *testing.T) {
	mgr, graph, _ := setupManager()
	ctx := context.Background()

	graph.Link("user-1", identity.Signal{Type: "email", Value: "hash123"})

	mgr.OptOut(ctx, "user-1", LevelNoTracking, "platform")

	// Nothing allowed
	if mgr.CheckConsent(ctx, "user-1", PurposeMeasurement) {
		t.Error("measurement should be blocked at Level 2")
	}

	// Identity graph should be deleted
	profile := graph.Profile("user-1")
	if profile != nil {
		t.Error("identity graph should be deleted at Level 2")
	}
}

func TestOptOut_Level3(t *testing.T) {
	mgr, graph, audiences := setupManager()
	ctx := context.Background()

	graph.Link("user-1", identity.Signal{Type: "email", Value: "hash123"})
	audiences.CreateSegment(ctx, audience.Segment{ID: "seg-1", AccountID: "a"})
	audiences.AddUsers(ctx, "seg-1", []string{"user-1"})

	result := mgr.OptOut(ctx, "user-1", LevelFullDeletion, "gdpr")

	if result.SegmentsRemoved != 1 {
		t.Errorf("segments_removed = %d, want 1", result.SegmentsRemoved)
	}

	// Verify deletion
	vr := mgr.VerifyDeletion(ctx, "user-1")
	if vr.Status != "verified" {
		t.Errorf("verification status = %s, want verified (failed: %v)", vr.Status, vr.FailedSystems)
	}
}

func TestVerifyDeletion_NotFound(t *testing.T) {
	mgr, _, _ := setupManager()
	vr := mgr.VerifyDeletion(context.Background(), "nonexistent")
	if vr.Status != "not_found" {
		t.Errorf("status = %s, want not_found", vr.Status)
	}
}

func TestStats(t *testing.T) {
	mgr, _, _ := setupManager()
	ctx := context.Background()

	mgr.OptOut(ctx, "u1", LevelNoPersonalisation, "cmp")
	mgr.OptOut(ctx, "u2", LevelNoTracking, "platform")
	mgr.OptOut(ctx, "u3", LevelFullDeletion, "gdpr")

	stats := mgr.Stats()
	if stats.Total != 3 {
		t.Errorf("total = %d, want 3", stats.Total)
	}
	if stats.Level1 != 1 || stats.Level2 != 1 || stats.Level3 != 1 {
		t.Errorf("levels = %d/%d/%d, want 1/1/1", stats.Level1, stats.Level2, stats.Level3)
	}
}
