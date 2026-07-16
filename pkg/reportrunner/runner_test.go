package reportrunner

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

func TestDue(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { tt := now.Add(-d); return &tt }

	cases := []struct {
		name     string
		schedule string
		lastRun  *time.Time
		want     bool
	}{
		{"daily never run", "@daily", nil, true},
		{"daily 25h ago", "@daily", ago(25 * time.Hour), true},
		{"daily 23h ago", "@daily", ago(23 * time.Hour), false},
		{"hourly 90m ago", "hourly", ago(90 * time.Minute), true},
		{"hourly 30m ago", "@hourly", ago(30 * time.Minute), false},
		{"weekly 8d ago", "@weekly", ago(8 * 24 * time.Hour), true},
		{"weekly 3d ago", "weekly", ago(3 * 24 * time.Hour), false},
		{"monthly never", "@monthly", nil, true},
		// 5-field cron (calendar-aligned; now = a Wednesday-noon fixture):
		// due when an activation passed since last_run.
		{"cron never run is due", "0 9 * * 1", nil, true},
		{"cron activation passed since last run", "0 9 * * *", ago(26 * time.Hour), true},
		{"cron no activation yet", "0 9 * * *", ago(time.Hour), false},
		{"garbage schedule never auto-runs", "every-fortnight", nil, false},
		{"empty schedule never runs", "", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Due(tc.schedule, tc.lastRun, now); got != tc.want {
				t.Errorf("Due(%q, %v) = %v, want %v", tc.schedule, tc.lastRun, got, tc.want)
			}
		})
	}
}

type fakeStore struct {
	reports []ScheduledReport
	marked  []string
}

func (f *fakeStore) ScheduledReports(context.Context) ([]ScheduledReport, error) {
	return f.reports, nil
}
func (f *fakeStore) MarkRun(_ context.Context, id string, _ time.Time) error {
	f.marked = append(f.marked, id)
	return nil
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(discard{}, nil)) }

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func TestRunDue_EnqueuesAndMarksDueReports(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	dayAgo := now.Add(-25 * time.Hour)
	hourAgo := now.Add(-30 * time.Minute)

	store := &fakeStore{reports: []ScheduledReport{
		// due (daily, 25h ago)
		{ID: "r1", AccountID: "acc-1", Name: "Daily spend", Schedule: "@daily", Delivery: "email", Format: "csv",
			QueryConfig: analytics.QueryParams{Table: "impressions"}, LastRun: &dayAgo, Recipient: "a@x.test"},
		// not due (daily but only 30m ago)
		{ID: "r2", AccountID: "acc-2", Name: "Recent", Schedule: "@daily", Delivery: "email",
			LastRun: &hourAgo, Recipient: "b@x.test"},
		// garbage schedule → skipped (cron expressions are VALID now)
		{ID: "r3", AccountID: "acc-3", Name: "Junk", Schedule: "every-fortnight", Delivery: "email", Recipient: "c@x.test"},
	}}

	var enqueued []ScheduledReport
	var enqueuedAt time.Time
	r := &Runner{
		Store: store,
		Enqueue: func(_ context.Context, rep ScheduledReport, now time.Time) error {
			enqueued = append(enqueued, rep)
			enqueuedAt = now
			return nil
		},
		Now: func() time.Time { return now },
		Log: quietLog(),
	}

	ran, err := r.RunDue(context.Background())
	if err != nil {
		t.Fatalf("RunDue: %v", err)
	}
	if ran != 1 {
		t.Fatalf("ran = %d, want 1 (only r1 is due)", ran)
	}
	if len(store.marked) != 1 || store.marked[0] != "r1" {
		t.Errorf("marked = %v, want [r1]", store.marked)
	}
	if len(enqueued) != 1 || enqueued[0].ID != "r1" || enqueued[0].Format != "csv" {
		t.Fatalf("enqueued = %+v, want r1 with csv format", enqueued)
	}
	if !enqueuedAt.Equal(now) {
		t.Errorf("enqueued at %v, want %v", enqueuedAt, now)
	}
}

func TestRunDue_MissingRecipientDoesNotMark(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{reports: []ScheduledReport{
		{ID: "r1", AccountID: "acc-1", Name: "No recipient", Schedule: "@daily", Delivery: "email", LastRun: nil, Recipient: ""},
	}}
	enqueues := 0
	r := &Runner{
		Store:   store,
		Enqueue: func(context.Context, ScheduledReport, time.Time) error { enqueues++; return nil },
		Now:     func() time.Time { return now },
		Log:     quietLog(),
	}
	ran, _ := r.RunDue(context.Background())
	if ran != 0 || len(store.marked) != 0 || enqueues != 0 {
		t.Errorf("ran=%d marked=%v enqueues=%d, want all zero (no recipient → failure, not marked)",
			ran, store.marked, enqueues)
	}
}

func TestRunDue_NoneDeliveryNeedsNoRecipient(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{reports: []ScheduledReport{
		{ID: "r1", AccountID: "acc-1", Name: "Artifact only", Schedule: "@daily", Delivery: "none", LastRun: nil},
	}}
	enqueues := 0
	r := &Runner{
		Store:   store,
		Enqueue: func(context.Context, ScheduledReport, time.Time) error { enqueues++; return nil },
		Now:     func() time.Time { return now },
		Log:     quietLog(),
	}
	ran, _ := r.RunDue(context.Background())
	if ran != 1 || enqueues != 1 || len(store.marked) != 1 {
		t.Errorf("ran=%d enqueues=%d marked=%v, want 1/1/[r1] (delivery=none schedules run without a recipient)",
			ran, enqueues, store.marked)
	}
}

func TestRunDue_FailedEnqueueNotMarked(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{reports: []ScheduledReport{
		{ID: "r1", AccountID: "acc-1", Name: "Broken", Schedule: "@daily", Delivery: "none", LastRun: nil},
	}}
	r := &Runner{
		Store:   store,
		Enqueue: func(context.Context, ScheduledReport, time.Time) error { return errors.New("queue down") },
		Now:     func() time.Time { return now },
		Log:     quietLog(),
	}
	ran, err := r.RunDue(context.Background())
	if err != nil {
		t.Fatalf("RunDue returned error: %v (individual failures must not bubble)", err)
	}
	if ran != 0 || len(store.marked) != 0 {
		t.Errorf("ran=%d marked=%v, want 0 and none (failed enqueue retries next tick)", ran, store.marked)
	}
}
