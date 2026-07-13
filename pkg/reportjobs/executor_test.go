package reportjobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/email"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/fs"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testExecutor(t *testing.T, store JobStore, query func(context.Context, string, analytics.QueryParams) (analytics.QueryResult, error)) (*Executor, *email.MemorySender) {
	t.Helper()
	objStore, err := fs.New(t.TempDir())
	if err != nil {
		t.Fatalf("fs store: %v", err)
	}
	mail := email.NewMemory(quietLog())
	return &Executor{
		Store:      store,
		Query:      query,
		Objects:    objStore,
		Bucket:     "adtech-reports",
		Email:      mail,
		From:       "reports@adtech.test",
		GatewayURL: "http://gw.test",
		Now:        time.Now,
		Log:        quietLog(),
	}, mail
}

func okQuery(_ context.Context, _ string, _ analytics.QueryParams) (analytics.QueryResult, error) {
	return analytics.QueryResult{
		Columns: []string{"campaign_id", "count"},
		Rows:    [][]any{{"camp-1", float64(5)}, {"camp-2", float64(2)}},
	}, nil
}

func TestExecutorRunOnce_Success(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryJobStore()
	exec, mail := testExecutor(t, store, okQuery)

	j := suiteJob("acc-1", "spend by campaign")
	j.Delivery = DeliveryEmail
	j.Recipient = "owner@x.test"
	id, _ := store.Enqueue(ctx, j)

	claimed, err := exec.RunOnce(ctx)
	if err != nil || !claimed {
		t.Fatalf("RunOnce = %v, %v; want claimed, nil", claimed, err)
	}

	got, _ := store.GetByAccount(ctx, "acc-1", id)
	if got.Status != StatusDone {
		t.Fatalf("status %s (error %q), want done", got.Status, got.Error)
	}
	if got.RowCount != 2 || got.ArtifactBytes == 0 {
		t.Errorf("artifact meta rows=%d bytes=%d", got.RowCount, got.ArtifactBytes)
	}
	if want := "acc-1/" + id + ".csv"; got.ArtifactKey != want {
		t.Errorf("artifact key %s, want %s", got.ArtifactKey, want)
	}

	// The artifact is really in the object store with the rendered content.
	rc, err := exec.Objects.Get(ctx, got.ArtifactBucket, got.ArtifactKey)
	if err != nil {
		t.Fatalf("artifact get: %v", err)
	}
	body, _ := io.ReadAll(rc)
	rc.Close()
	if !strings.Contains(string(body), "camp-1,5") {
		t.Errorf("artifact content:\n%s", body)
	}

	sent := mail.Sent()
	if len(sent) != 1 || sent[0].To != "owner@x.test" {
		t.Fatalf("sent = %+v, want one email to owner@x.test", sent)
	}
	if !strings.Contains(sent[0].Body, "http://gw.test/v1/api/reports/jobs/"+id+"/download") {
		t.Errorf("email body missing download link:\n%s", sent[0].Body)
	}
}

func TestExecutorRunOnce_EmptyQueue(t *testing.T) {
	exec, _ := testExecutor(t, NewMemoryJobStore(), okQuery)
	claimed, err := exec.RunOnce(context.Background())
	if claimed || err != nil {
		t.Errorf("RunOnce on empty queue = %v, %v; want false, nil", claimed, err)
	}
}

func TestExecutorRunOnce_QueryFailureMarksFailed(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryJobStore()
	exec, mail := testExecutor(t, store, func(context.Context, string, analytics.QueryParams) (analytics.QueryResult, error) {
		return analytics.QueryResult{}, errors.New("clickhouse melted")
	})

	j := suiteJob("acc-1", "doomed")
	j.Delivery = DeliveryEmail
	j.Recipient = "owner@x.test"
	id, _ := store.Enqueue(ctx, j)

	claimed, err := exec.RunOnce(ctx)
	if err != nil || !claimed {
		t.Fatalf("RunOnce = %v, %v; want claimed, nil (job failure is not a worker failure)", claimed, err)
	}
	got, _ := store.GetByAccount(ctx, "acc-1", id)
	if got.Status != StatusFailed || !strings.Contains(got.Error, "clickhouse melted") {
		t.Errorf("job after failure: status=%s error=%q", got.Status, got.Error)
	}
	if got.ArtifactKey != "" {
		t.Errorf("failed job has artifact %s", got.ArtifactKey)
	}
	if len(mail.Sent()) != 0 {
		t.Errorf("failed job sent %d emails, want 0", len(mail.Sent()))
	}
}

func TestExecutorRunOnce_BadFormatMarksFailed(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryJobStore()
	exec, _ := testExecutor(t, store, okQuery)

	j := suiteJob("acc-1", "bad format")
	j.Format = "xlsx" // gateway validates, but the executor must not trust it
	id, _ := store.Enqueue(ctx, j)

	if _, err := exec.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	got, _ := store.GetByAccount(ctx, "acc-1", id)
	if got.Status != StatusFailed || !strings.Contains(got.Error, "unsupported report format") {
		t.Errorf("status=%s error=%q", got.Status, got.Error)
	}
}

func TestExecutorRunOnce_DeliveryNoneSendsNothing(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryJobStore()
	exec, mail := testExecutor(t, store, okQuery)

	id, _ := store.Enqueue(ctx, suiteJob("acc-1", "quiet"))
	exec.RunOnce(ctx)
	got, _ := store.GetByAccount(ctx, "acc-1", id)
	if got.Status != StatusDone {
		t.Fatalf("status %s, want done", got.Status)
	}
	if len(mail.Sent()) != 0 {
		t.Errorf("delivery=none sent %d emails", len(mail.Sent()))
	}
}

func TestSweeperRemovesExpiredArtifactAndRow(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryJobStore()
	exec, _ := testExecutor(t, store, okQuery)

	// One expired job, one live job — sweep must only take the first.
	expired := suiteJob("acc-1", "old")
	expired.ExpiresAt = time.Now().Add(-time.Hour)
	expiredID, _ := store.Enqueue(ctx, expired)
	liveID, _ := store.Enqueue(ctx, suiteJob("acc-1", "fresh"))
	exec.RunOnce(ctx)
	exec.RunOnce(ctx)

	before, _ := store.GetByAccount(ctx, "acc-1", expiredID)
	if before == nil || before.ArtifactKey == "" {
		t.Fatalf("expired job not done with artifact: %+v", before)
	}

	sweeper := &Sweeper{Store: store, Objects: exec.Objects, Now: time.Now, Log: quietLog()}
	removed, err := sweeper.SweepOnce(ctx)
	if err != nil || removed != 1 {
		t.Fatalf("SweepOnce = %d, %v; want 1, nil", removed, err)
	}
	if got, _ := store.GetByAccount(ctx, "acc-1", expiredID); got != nil {
		t.Errorf("expired job row still present")
	}
	if exists, _ := exec.Objects.Exists(ctx, before.ArtifactBucket, before.ArtifactKey); exists {
		t.Errorf("expired artifact still in object store")
	}
	if got, _ := store.GetByAccount(ctx, "acc-1", liveID); got == nil || got.Status != StatusDone {
		t.Errorf("live job was swept: %+v", got)
	}
}
