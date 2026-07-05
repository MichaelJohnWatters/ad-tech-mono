package reportrunner

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/email"
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
		{"unknown schedule never auto-runs", "0 9 * * 1", nil, false},
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

func TestRunDue_EmailsAndMarksDueReports(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	dayAgo := now.Add(-25 * time.Hour)
	hourAgo := now.Add(-30 * time.Minute)

	store := &fakeStore{reports: []ScheduledReport{
		// due (daily, 25h ago)
		{ID: "r1", AccountID: "acc-1", Name: "Daily spend", Schedule: "@daily", Delivery: "email",
			QueryConfig: analytics.QueryParams{Table: "impressions"}, LastRun: &dayAgo, Recipient: "a@x.test"},
		// not due (daily but only 30m ago)
		{ID: "r2", AccountID: "acc-2", Name: "Recent", Schedule: "@daily", Delivery: "email",
			LastRun: &hourAgo, Recipient: "b@x.test"},
		// unknown schedule → skipped
		{ID: "r3", AccountID: "acc-3", Name: "Cron", Schedule: "0 9 * * 1", Delivery: "email", Recipient: "c@x.test"},
	}}

	mail := email.NewMemory(quietLog())
	var gotAccount string
	var gotFilters map[string]string
	r := &Runner{
		Store: store,
		Email: mail,
		From:  "reports@adtech.test",
		Now:   func() time.Time { return now },
		Log:   quietLog(),
		Query: func(_ context.Context, accountID string, p analytics.QueryParams) (analytics.QueryResult, error) {
			gotAccount = accountID
			gotFilters = p.Filters
			return analytics.QueryResult{Columns: []string{"day", "spend"}, Rows: [][]any{{"2026-07-04", 12.5}}}, nil
		},
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
	// The query was tenant-scoped to the report's account.
	if gotAccount != "acc-1" || gotFilters["account_id"] != "acc-1" {
		t.Errorf("query account=%q filter=%q, want acc-1", gotAccount, gotFilters["account_id"])
	}
	sent := mail.Sent()
	if len(sent) != 1 || sent[0].To != "a@x.test" || sent[0].From != "reports@adtech.test" {
		t.Fatalf("sent = %+v, want one email to a@x.test", sent)
	}
	if !contains(sent[0].Body, "Daily spend") || !contains(sent[0].Body, "12.5") {
		t.Errorf("email body missing report name/data: %q", sent[0].Body)
	}
}

func TestRunDue_MissingRecipientDoesNotMark(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{reports: []ScheduledReport{
		{ID: "r1", AccountID: "acc-1", Name: "No recipient", Schedule: "@daily", Delivery: "email", LastRun: nil, Recipient: ""},
	}}
	r := &Runner{
		Store: store, Email: email.NewMemory(quietLog()), From: "x@x.test",
		Now: func() time.Time { return now }, Log: quietLog(),
		Query: func(context.Context, string, analytics.QueryParams) (analytics.QueryResult, error) {
			return analytics.QueryResult{}, nil
		},
	}
	ran, _ := r.RunDue(context.Background())
	if ran != 0 || len(store.marked) != 0 {
		t.Errorf("ran=%d marked=%v, want 0 and none (no recipient → failure, not marked)", ran, store.marked)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && indexOf(s, sub) >= 0 }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
