package reportjobs

import (
	"context"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

// suiteJob returns a minimal valid job for store tests.
func suiteJob(account, name string) Job {
	return Job{
		AccountID: account,
		Name:      name,
		QueryConfig: analytics.QueryParams{
			Table:   "impressions",
			Metrics: []string{"count"},
			Filters: map[string]string{"account_id": account},
		},
		Format:   FormatCSV,
		Delivery: DeliveryNone,
		Source:   SourceManual,
	}
}

// runJobStoreSuite exercises JobStore semantics shared by the memory and
// Postgres implementations. accA/accB must be two distinct existing accounts.
func runJobStoreSuite(t *testing.T, store JobStore, accA, accB string) {
	ctx := context.Background()

	t.Run("lifecycle_enqueue_claim_done", func(t *testing.T) {
		id, err := store.Enqueue(ctx, suiteJob(accA, "lifecycle"))
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if id == "" {
			t.Fatal("enqueue returned empty id")
		}

		claimed, err := store.ClaimOne(ctx)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if claimed == nil || claimed.ID != id {
			t.Fatalf("claimed %+v, want id %s", claimed, id)
		}
		if claimed.Status != StatusRunning || claimed.Attempts != 1 {
			t.Errorf("claimed status=%s attempts=%d, want running/1", claimed.Status, claimed.Attempts)
		}

		art := Artifact{Bucket: "adtech-reports", Key: accA + "/" + id + ".csv", Bytes: 42, Rows: 3}
		if err := store.MarkDone(ctx, id, art); err != nil {
			t.Fatalf("mark done: %v", err)
		}
		got, err := store.GetByAccount(ctx, accA, id)
		if err != nil || got == nil {
			t.Fatalf("get: %v (job=%v)", err, got)
		}
		if got.Status != StatusDone || got.ArtifactKey != art.Key || got.RowCount != 3 {
			t.Errorf("after done: %+v", got)
		}
		if got.FinishedAt == nil {
			t.Error("finished_at not set")
		}
	})

	t.Run("claim_empty_queue_returns_nil", func(t *testing.T) {
		for { // drain
			j, err := store.ClaimOne(ctx)
			if err != nil {
				t.Fatalf("drain claim: %v", err)
			}
			if j == nil {
				break
			}
			if err := store.MarkFailed(ctx, j.ID, "drained by test"); err != nil {
				t.Fatalf("drain fail: %v", err)
			}
		}
		j, err := store.ClaimOne(ctx)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if j != nil {
			t.Fatalf("claimed %+v from empty queue", j)
		}
	})

	t.Run("claim_is_fifo", func(t *testing.T) {
		first, err := store.Enqueue(ctx, suiteJob(accA, "fifo-1"))
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		second, err := store.Enqueue(ctx, suiteJob(accA, "fifo-2"))
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		c1, _ := store.ClaimOne(ctx)
		c2, _ := store.ClaimOne(ctx)
		if c1 == nil || c2 == nil || c1.ID != first || c2.ID != second {
			t.Errorf("claim order got (%v, %v), want (%s, %s)", c1, c2, first, second)
		}
		store.MarkDone(ctx, first, Artifact{})
		store.MarkDone(ctx, second, Artifact{})
	})

	t.Run("mark_failed_records_error", func(t *testing.T) {
		id, _ := store.Enqueue(ctx, suiteJob(accA, "failing"))
		c, _ := store.ClaimOne(ctx)
		if c == nil || c.ID != id {
			t.Fatalf("claim got %v, want %s", c, id)
		}
		if err := store.MarkFailed(ctx, id, "query exploded"); err != nil {
			t.Fatalf("mark failed: %v", err)
		}
		got, _ := store.GetByAccount(ctx, accA, id)
		if got == nil || got.Status != StatusFailed || got.Error != "query exploded" {
			t.Errorf("after fail: %+v", got)
		}
	})

	t.Run("schedule_enqueue_dedupes_active", func(t *testing.T) {
		j := suiteJob(accA, "scheduled")
		j.Source = SourceSchedule
		j.SavedReportID = savedReportIDForSuite(t, store, accA)
		id1, err := store.Enqueue(ctx, j)
		if err != nil {
			t.Fatalf("enqueue 1: %v", err)
		}
		if id1 == "" {
			t.Fatal("first schedule enqueue deduped, want insert")
		}
		id2, err := store.Enqueue(ctx, j)
		if err != nil {
			t.Fatalf("enqueue 2: %v", err)
		}
		if id2 != "" {
			t.Errorf("second schedule enqueue got id %s, want dedupe (empty)", id2)
		}
		// Once the active job completes, a new schedule enqueue is allowed.
		c, _ := store.ClaimOne(ctx)
		if c == nil || c.ID != id1 {
			t.Fatalf("claim got %v, want %s", c, id1)
		}
		store.MarkDone(ctx, id1, Artifact{})
		id3, err := store.Enqueue(ctx, j)
		if err != nil || id3 == "" {
			t.Errorf("enqueue after done: id=%q err=%v, want new id", id3, err)
		}
		if id3 != "" {
			c, _ := store.ClaimOne(ctx)
			if c != nil {
				store.MarkDone(ctx, c.ID, Artifact{})
			}
		}
	})

	t.Run("tenant_filtering", func(t *testing.T) {
		idA, _ := store.Enqueue(ctx, suiteJob(accA, "tenant-a"))
		if got, err := store.GetByAccount(ctx, accB, idA); err != nil || got != nil {
			t.Errorf("cross-tenant get: %v, %v — want nil, nil", got, err)
		}
		listB, err := store.ListByAccount(ctx, accB, 50)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for _, j := range listB {
			if j.AccountID != accB {
				t.Errorf("account B list contains foreign job %s (account %s)", j.ID, j.AccountID)
			}
		}
		c, _ := store.ClaimOne(ctx)
		if c != nil {
			store.MarkDone(ctx, c.ID, Artifact{})
		}
	})

	t.Run("expired_and_delete", func(t *testing.T) {
		j := suiteJob(accA, "short-lived")
		j.ExpiresAt = time.Now().Add(-time.Hour)
		id, _ := store.Enqueue(ctx, j)
		c, _ := store.ClaimOne(ctx)
		if c == nil || c.ID != id {
			t.Fatalf("claim got %v, want %s", c, id)
		}
		store.MarkDone(ctx, id, Artifact{Bucket: "b", Key: "k"})

		exp, err := store.Expired(ctx, time.Now(), 100)
		if err != nil {
			t.Fatalf("expired: %v", err)
		}
		found := false
		for _, e := range exp {
			if e.ID == id {
				found = true
			}
			if e.Status != StatusDone && e.Status != StatusFailed {
				t.Errorf("expired returned %s job %s", e.Status, e.ID)
			}
		}
		if !found {
			t.Errorf("expired did not return job %s", id)
		}
		if err := store.Delete(ctx, id); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if got, _ := store.GetByAccount(ctx, accA, id); got != nil {
			t.Errorf("job %s still present after delete", id)
		}
	})

	t.Run("reclaim_expired", func(t *testing.T) {
		id, _ := store.Enqueue(ctx, suiteJob(accA, "stuck"))
		c, _ := store.ClaimOne(ctx)
		if c == nil || c.ID != id {
			t.Fatalf("claim got %v, want %s", c, id)
		}
		// Heartbeating keeps the lease alive conceptually; the memory store
		// treats any running job as expired (tests drive timing), so a
		// reclaim hands the job back.
		if err := store.ExtendLease(ctx, id); err != nil {
			t.Fatalf("extend lease: %v", err)
		}
		n, err := store.ReclaimExpired(ctx)
		if err != nil {
			t.Fatalf("reclaim: %v", err)
		}
		if n != 1 {
			t.Errorf("reclaimed %d, want 1", n)
		}
		got, _ := store.GetByAccount(ctx, accA, id)
		if got == nil || got.Status != StatusQueued {
			t.Errorf("after requeue: %+v, want queued", got)
		}
		c, _ = store.ClaimOne(ctx)
		if c != nil {
			store.MarkDone(ctx, c.ID, Artifact{})
		}
	})
}

// savedReportIDForSuite returns a saved_report id valid for the store under
// test: the Postgres store has an FK to saved_reports, so its harness
// registers a real row; memory has no FK.
var savedReportIDHook func(t *testing.T, accountID string) string

func savedReportIDForSuite(t *testing.T, store JobStore, accountID string) string {
	if _, ok := store.(*MemoryJobStore); ok {
		return "sr-1"
	}
	if savedReportIDHook == nil {
		t.Fatal("savedReportIDHook not set for non-memory store")
	}
	return savedReportIDHook(t, accountID)
}

func TestMemoryJobStore(t *testing.T) {
	runJobStoreSuite(t, NewMemoryJobStore(), "acc-a", "acc-b")
}
