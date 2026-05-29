package audience

import (
	"context"
	"testing"
)

func TestStore_CreateAndQuery(t *testing.T) {
	store := NewStore()
	ctx := context.Background()

	store.CreateSegment(ctx, Segment{
		ID: "seg-1", AccountID: "adv-1", Name: "High Value Customers",
		Type: SegmentFirstParty,
	})

	store.AddUsers(ctx, "seg-1", []string{"user-a", "user-b", "user-c"})

	seg := store.GetSegment(ctx, "seg-1")
	if seg == nil {
		t.Fatal("expected segment")
	}
	if seg.Size != 3 {
		t.Errorf("size = %d, want 3", seg.Size)
	}

	if !store.IsInSegment(ctx, "seg-1", "user-a") {
		t.Error("expected user-a in segment")
	}
	if store.IsInSegment(ctx, "seg-1", "user-z") {
		t.Error("unexpected user-z in segment")
	}
}

func TestStore_AccessControl(t *testing.T) {
	store := NewStore()
	ctx := context.Background()

	store.CreateSegment(ctx, Segment{
		ID: "seg-private", AccountID: "adv-1", Name: "Private Segment",
	})
	store.CreateSegment(ctx, Segment{
		ID: "seg-shared", AccountID: "adv-1", Name: "Shared Segment", Shared: true,
	})

	store.AddUsers(ctx, "seg-private", []string{"user-1"})
	store.AddUsers(ctx, "seg-shared", []string{"user-1"})

	// adv-1 sees both
	segs1 := store.SegmentsForUser(ctx, "user-1", "adv-1")
	if len(segs1) != 2 {
		t.Errorf("adv-1 should see 2 segments, got %d", len(segs1))
	}

	// adv-2 only sees shared
	segs2 := store.SegmentsForUser(ctx, "user-1", "adv-2")
	if len(segs2) != 1 {
		t.Errorf("adv-2 should see 1 segment, got %d", len(segs2))
	}
}

func TestStore_CompositeSegment(t *testing.T) {
	store := NewStore()
	ctx := context.Background()

	store.CreateSegment(ctx, Segment{ID: "seg-a", AccountID: "a"})
	store.CreateSegment(ctx, Segment{ID: "seg-b", AccountID: "a"})
	store.CreateSegment(ctx, Segment{ID: "seg-c", AccountID: "a"})

	store.AddUsers(ctx, "seg-a", []string{"user-1", "user-2"})
	store.AddUsers(ctx, "seg-b", []string{"user-1"})
	store.AddUsers(ctx, "seg-c", []string{"user-2"})

	// AND: user-1 is in both A and B
	if !store.EvaluateComposite(ctx, CompositeRule{Operator: "and", Segments: []string{"seg-a", "seg-b"}}, "user-1") {
		t.Error("expected user-1 in A AND B")
	}
	if store.EvaluateComposite(ctx, CompositeRule{Operator: "and", Segments: []string{"seg-a", "seg-b"}}, "user-2") {
		t.Error("user-2 is not in B, should fail A AND B")
	}

	// OR: user-2 is in A or C
	if !store.EvaluateComposite(ctx, CompositeRule{Operator: "or", Segments: []string{"seg-b", "seg-c"}}, "user-2") {
		t.Error("expected user-2 in B OR C")
	}

	// AND_NOT: user-2 is in A but NOT B
	if !store.EvaluateComposite(ctx, CompositeRule{Operator: "and_not", Segments: []string{"seg-a", "seg-b"}}, "user-2") {
		t.Error("expected user-2 in A AND NOT B")
	}
}

func TestStore_RemoveUserFromAll(t *testing.T) {
	store := NewStore()
	ctx := context.Background()

	store.CreateSegment(ctx, Segment{ID: "s1", AccountID: "a"})
	store.CreateSegment(ctx, Segment{ID: "s2", AccountID: "a"})
	store.AddUsers(ctx, "s1", []string{"user-1"})
	store.AddUsers(ctx, "s2", []string{"user-1"})

	removed := store.RemoveUserFromAll(ctx, "user-1")
	if removed != 2 {
		t.Errorf("removed = %d, want 2", removed)
	}
	if store.IsInSegment(ctx, "s1", "user-1") || store.IsInSegment(ctx, "s2", "user-1") {
		t.Error("user should be removed from all segments")
	}
}
