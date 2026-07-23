package ingestjobs

import (
	"context"
	"testing"
	"time"
)

func newJob(bucket, key string) Job {
	return Job{
		AccountID:   "acc-1",
		Source:      SourceDropzone,
		Provider:    "acme",
		FileBucket:  bucket,
		FileKey:     key,
		SegmentSpec: SegmentSpec{Name: "seg", IDType: "user_id"},
	}
}

func TestEnqueueClaimHappyPath(t *testing.T) {
	m := NewMemoryIngestStore()
	ctx := context.Background()

	id, err := m.Enqueue(ctx, newJob("b", "incoming/f.csv"))
	if err != nil || id == "" {
		t.Fatalf("enqueue: id=%q err=%v", id, err)
	}

	j, err := m.ClaimOne(ctx)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if j == nil || j.ID != id {
		t.Fatalf("claim returned %+v, want job %s", j, id)
	}
	if j.Status != StatusRunning {
		t.Errorf("claimed status = %s, want running", j.Status)
	}
	if j.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", j.Attempts)
	}
	if j.SegmentSpec.IDType != "user_id" {
		t.Errorf("segment spec not round-tripped: %+v", j.SegmentSpec)
	}

	// Nothing else to claim.
	if j2, _ := m.ClaimOne(ctx); j2 != nil {
		t.Errorf("second claim returned %+v, want nil", j2)
	}

	if err := m.MarkDone(ctx, id, IngestResult{
		SegmentID: "seg-1", TotalRows: 10, ValidRows: 9, RejectedRows: 1,
		MatchedRows: 5, MatchRate: 0.5, RejectedKey: "rej/f.csv",
	}); err != nil {
		t.Fatalf("markdone: %v", err)
	}
	got, _ := m.GetByAccount(ctx, "acc-1", id)
	if got == nil || got.Status != StatusDone {
		t.Fatalf("after done: %+v", got)
	}
	if got.MatchedRows != 5 || got.MatchRate != 0.5 || got.SegmentID != "seg-1" {
		t.Errorf("result counts not recorded: %+v", got)
	}
}

func TestClaimByID(t *testing.T) {
	m := NewMemoryIngestStore()
	ctx := context.Background()

	id, err := m.Enqueue(ctx, newJob("b", "inline.csv"))
	if err != nil || id == "" {
		t.Fatalf("enqueue: id=%q err=%v", id, err)
	}

	// Claim the specific row (the gateway inline path).
	j, err := m.ClaimByID(ctx, id)
	if err != nil {
		t.Fatalf("claim by id: %v", err)
	}
	if j == nil || j.ID != id || j.Status != StatusRunning || j.Attempts != 1 {
		t.Fatalf("claim by id returned %+v", j)
	}

	// Re-claiming the now-running row returns nil (not queued).
	if j2, _ := m.ClaimByID(ctx, id); j2 != nil {
		t.Errorf("second claim-by-id returned %+v, want nil", j2)
	}
	// ClaimOne must not also take it (it's running).
	if j3, _ := m.ClaimOne(ctx); j3 != nil {
		t.Errorf("ClaimOne took a running job: %+v", j3)
	}
	// Unknown id → nil.
	if j4, _ := m.ClaimByID(ctx, "ingest-999"); j4 != nil {
		t.Errorf("claim of unknown id returned %+v, want nil", j4)
	}
}

func TestClaimSkipsFutureRunAt(t *testing.T) {
	base := time.Now()
	m := NewMemoryIngestStore()
	m.Now = func() time.Time { return base }
	ctx := context.Background()

	// A job scheduled for the future is staged but not yet claimable.
	future := newJob("b", "held.csv")
	future.RunAt = base.Add(time.Hour)
	if _, err := m.Enqueue(ctx, future); err != nil {
		t.Fatalf("enqueue held: %v", err)
	}

	if j, _ := m.ClaimOne(ctx); j != nil {
		t.Fatalf("claimed a not-yet-due job: %+v", j)
	}

	// Advance past run_at → now claimable.
	m.Now = func() time.Time { return base.Add(2 * time.Hour) }
	j, err := m.ClaimOne(ctx)
	if err != nil {
		t.Fatalf("claim after due: %v", err)
	}
	if j == nil {
		t.Fatal("due job not claimed after run_at passed")
	}
}

func TestReclaimExpiredLease(t *testing.T) {
	base := time.Now()
	m := NewMemoryIngestStore()
	m.Now = func() time.Time { return base }
	ctx := context.Background()

	id, _ := m.Enqueue(ctx, newJob("b", "f.csv"))
	if _, err := m.ClaimOne(ctx); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// Lease still live → not reclaimed.
	if n, _ := m.ReclaimExpired(ctx); n != 0 {
		t.Errorf("reclaimed %d live-lease jobs, want 0", n)
	}

	// Advance past the lease TTL → reclaimed back to queued.
	m.Now = func() time.Time { return base.Add(LeaseTTL + time.Second) }
	n, err := m.ReclaimExpired(ctx)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if n != 1 {
		t.Errorf("reclaimed %d, want 1", n)
	}
	got, _ := m.GetByAccount(ctx, "acc-1", id)
	if got.Status != StatusQueued {
		t.Errorf("after reclaim status = %s, want queued", got.Status)
	}
	// Re-claimable now.
	if j, _ := m.ClaimOne(ctx); j == nil {
		t.Error("reclaimed job not re-claimable")
	}
}

func TestNotifyEmailsRoundTrip(t *testing.T) {
	m := NewMemoryIngestStore()
	ctx := context.Background()

	j := newJob("b", "notify.csv")
	j.NotifyEmails = []string{"a@example.com", "b@example.com"}
	id, err := m.Enqueue(ctx, j)
	if err != nil || id == "" {
		t.Fatalf("enqueue: id=%q err=%v", id, err)
	}
	claimed, err := m.ClaimOne(ctx)
	if err != nil || claimed == nil {
		t.Fatalf("claim: %+v err=%v", claimed, err)
	}
	if len(claimed.NotifyEmails) != 2 || claimed.NotifyEmails[0] != "a@example.com" {
		t.Errorf("notify_emails not round-tripped onto claimed job: %+v", claimed.NotifyEmails)
	}
}

func TestDedupeOnBucketKey(t *testing.T) {
	m := NewMemoryIngestStore()
	ctx := context.Background()

	id1, err := m.Enqueue(ctx, newJob("b", "dup.csv"))
	if err != nil || id1 == "" {
		t.Fatalf("first enqueue: id=%q err=%v", id1, err)
	}
	// Same (bucket, key) while first is still queued → dedupe.
	id2, err := m.Enqueue(ctx, newJob("b", "dup.csv"))
	if err != nil {
		t.Fatalf("dup enqueue: %v", err)
	}
	if id2 != "" {
		t.Errorf("dedupe failed: second enqueue returned %q, want \"\"", id2)
	}

	// Different key → distinct job.
	id3, _ := m.Enqueue(ctx, newJob("b", "other.csv"))
	if id3 == "" || id3 == id1 {
		t.Errorf("distinct file got id %q (id1=%q)", id3, id1)
	}

	// Once the first is terminal, the same key can be re-enqueued.
	if _, err := m.ClaimOne(ctx); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// Claim order is run_at/created — drain both then finish the dup one.
	if err := m.MarkDone(ctx, id1, IngestResult{}); err != nil {
		t.Fatalf("markdone: %v", err)
	}
	id4, err := m.Enqueue(ctx, newJob("b", "dup.csv"))
	if err != nil || id4 == "" {
		t.Errorf("re-enqueue after terminal: id=%q err=%v", id4, err)
	}
}
