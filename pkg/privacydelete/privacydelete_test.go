package privacydelete

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeStore is an in-memory Store: a set of pending users, residual data per
// user, and captured completed/verified marks.
type fakeStore struct {
	pending     []string
	unverified  []string
	residual    map[string][]string // user -> systems still holding data
	purgeErr    map[string]error
	completed   map[string]Purge
	verified    map[string]bool
	purgedUsers []string
}

func (f *fakeStore) PendingDeletions(context.Context) ([]string, error) { return f.pending, nil }
func (f *fakeStore) CompletedUnverified(context.Context) ([]string, error) {
	return f.unverified, nil
}

func (f *fakeStore) PurgeUser(_ context.Context, userID string) (Purge, error) {
	if err := f.purgeErr[userID]; err != nil {
		return Purge{}, err
	}
	f.purgedUsers = append(f.purgedUsers, userID)
	delete(f.residual, userID) // purge clears residual data
	return Purge{IdentityEdges: 2, SegmentMembers: 1}, nil
}

func (f *fakeStore) MarkCompleted(_ context.Context, userID string, p Purge) error {
	if f.completed == nil {
		f.completed = map[string]Purge{}
	}
	f.completed[userID] = p
	return nil
}

func (f *fakeStore) Residual(_ context.Context, userID string) ([]string, error) {
	return f.residual[userID], nil
}

func (f *fakeStore) MarkVerified(_ context.Context, userID string) error {
	if f.verified == nil {
		f.verified = map[string]bool{}
	}
	f.verified[userID] = true
	return nil
}

type capturingBus struct{ published []string }

func (c *capturingBus) Publish(_ context.Context, subject string, _ []byte) error {
	c.published = append(c.published, subject)
	return nil
}

func TestRunPending_PurgesMarksAnnounces(t *testing.T) {
	store := &fakeStore{pending: []string{"u1", "u2"}, residual: map[string][]string{}}
	bus := &capturingBus{}
	d := &Deleter{Store: store, Announce: bus, Subject: "adtech.privacy.deletion_completed", Log: quietLog()}

	n, err := d.RunPending(context.Background())
	if err != nil {
		t.Fatalf("RunPending: %v", err)
	}
	if n != 2 {
		t.Errorf("completed = %d, want 2", n)
	}
	if len(store.purgedUsers) != 2 {
		t.Errorf("purged %d users, want 2", len(store.purgedUsers))
	}
	if _, ok := store.completed["u1"]; !ok {
		t.Error("u1 not marked completed")
	}
	if len(bus.published) != 2 {
		t.Errorf("announced %d completions, want 2", len(bus.published))
	}
}

func TestRunPending_PurgeFailureSkipsAndContinues(t *testing.T) {
	store := &fakeStore{
		pending:  []string{"good", "bad", "good2"},
		residual: map[string][]string{},
		purgeErr: map[string]error{"bad": errors.New("db blip")},
	}
	d := &Deleter{Store: store, Log: quietLog()}

	n, err := d.RunPending(context.Background())
	if err != nil {
		t.Fatalf("RunPending: %v", err)
	}
	if n != 2 {
		t.Errorf("completed = %d, want 2 (bad skipped)", n)
	}
	if _, ok := store.completed["bad"]; ok {
		t.Error("failed user must not be marked completed (so it retries next run)")
	}
}

func TestRunPending_NoAnnouncerIsFine(t *testing.T) {
	store := &fakeStore{pending: []string{"u1"}, residual: map[string][]string{}}
	d := &Deleter{Store: store, Log: quietLog()} // no Announce
	if n, err := d.RunPending(context.Background()); err != nil || n != 1 {
		t.Fatalf("RunPending = %d, %v; want 1, nil", n, err)
	}
}

func TestVerifier_MarksCleanUsersVerified(t *testing.T) {
	store := &fakeStore{
		unverified: []string{"clean", "dirty"},
		residual:   map[string][]string{"dirty": {SystemSegmentMembers}},
	}
	v := &Verifier{Store: store, Log: quietLog()}

	verified, incomplete, err := v.RunUnverified(context.Background())
	if err != nil {
		t.Fatalf("RunUnverified: %v", err)
	}
	if verified != 1 || incomplete != 1 {
		t.Errorf("verified=%d incomplete=%d, want 1 and 1", verified, incomplete)
	}
	if !store.verified["clean"] {
		t.Error("clean user should be marked verified")
	}
	if store.verified["dirty"] {
		t.Error("user with residual data must NOT be marked verified")
	}
}
