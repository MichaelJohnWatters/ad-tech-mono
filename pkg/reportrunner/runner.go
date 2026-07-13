// Package reportrunner turns due scheduled reports into report jobs.
//
// It is the runtime behind the saved_reports.schedule field: a report with an
// interval schedule (@hourly/@daily/@weekly/@monthly) is enqueued on its
// cadence as an async report job (pkg/reportjobs) — executed by the worker,
// stored as a downloadable artifact, and emailed as a link when the report's
// delivery is email. The report-runner service ticks RunDue periodically.
//
// The schedule model is interval-based (not full cron) to avoid a parser
// dependency: "due" means now − last_run ≥ the schedule's interval. last_run
// is stamped at enqueue time, so the cadence is enqueue-to-enqueue.
package reportrunner

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

// ScheduledReport is a saved_reports row the runner may enqueue, joined with
// the recipient (account owner email) resolved from team_members.
type ScheduledReport struct {
	ID          string
	AccountID   string
	Name        string
	Schedule    string
	Delivery    string // email | webhook | none
	Format      string // csv | json | parquet
	QueryConfig analytics.QueryParams
	LastRun     *time.Time // nil = never run
	Recipient   string     // owner email for delivery=email
}

// Store reads schedulable reports and records runs.
type Store interface {
	// ScheduledReports returns every report with a non-empty schedule and
	// email delivery, joined with its recipient. Due-filtering is the runner's
	// job (so it stays unit-testable).
	ScheduledReports(ctx context.Context) ([]ScheduledReport, error)
	// MarkRun records that a report ran at t.
	MarkRun(ctx context.Context, id string, t time.Time) error
}

// QueryFunc runs a report query for an account and returns the result. The
// implementation posts to the reporting service; it is a func type so tests
// can fake it. Used by the job executor (pkg/reportjobs).
type QueryFunc func(ctx context.Context, accountID string, params analytics.QueryParams) (analytics.QueryResult, error)

// EnqueueFunc submits a due scheduled report as an async report job. The
// implementation (wired in cmd/report-runner) resolves tenant scope filters
// and inserts into the job queue; it is a func type — not a pkg/reportjobs
// interface — so neither package imports the other.
type EnqueueFunc func(ctx context.Context, rep ScheduledReport, now time.Time) error

// Runner enqueues due reports as jobs.
type Runner struct {
	Store   Store
	Enqueue EnqueueFunc
	Now     func() time.Time
	Log     *slog.Logger
}

// RunDue enqueues every report that is currently due and returns how many
// were enqueued. Individual failures are logged and skipped — one bad report
// never blocks the rest, and a failed enqueue is NOT marked (so it retries
// next tick).
func (r *Runner) RunDue(ctx context.Context) (int, error) {
	now := r.Now()
	reports, err := r.Store.ScheduledReports(ctx)
	if err != nil {
		return 0, fmt.Errorf("load scheduled reports: %w", err)
	}
	ran := 0
	for _, rep := range reports {
		if !Due(rep.Schedule, rep.LastRun, now) {
			continue
		}
		if err := r.runOne(ctx, rep, now); err != nil {
			r.Log.Error("scheduled report enqueue failed", "report", rep.ID, "name", rep.Name, "error", err)
			continue
		}
		ran++
	}
	return ran, nil
}

func (r *Runner) runOne(ctx context.Context, rep ScheduledReport, now time.Time) error {
	if rep.Delivery == "email" && rep.Recipient == "" {
		return fmt.Errorf("no recipient email for account %s", rep.AccountID)
	}
	if err := r.Enqueue(ctx, rep, now); err != nil {
		return fmt.Errorf("enqueue: %w", err)
	}
	// MarkRun at enqueue (not completion): the cadence is enqueue-to-enqueue,
	// and the job queue's schedule-dedupe already prevents pile-ups if a job
	// is still running when the next interval arrives.
	if err := r.Store.MarkRun(ctx, rep.ID, now); err != nil {
		return fmt.Errorf("mark run: %w", err)
	}
	r.Log.Info("scheduled report enqueued", "report", rep.ID, "name", rep.Name,
		"delivery", rep.Delivery, "format", rep.Format)
	return nil
}

// Due reports whether a report on the given schedule is due to run now. An
// unrecognised schedule is never auto-run (returns false); a never-run report
// on a known schedule is due immediately.
func Due(schedule string, lastRun *time.Time, now time.Time) bool {
	interval, ok := scheduleInterval(schedule)
	if !ok {
		return false
	}
	if lastRun == nil {
		return true
	}
	return now.Sub(*lastRun) >= interval
}

// scheduleInterval maps an interval keyword (with or without the "@" cron-macro
// prefix, case-insensitive) to its duration.
func scheduleInterval(s string) (time.Duration, bool) {
	switch strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "@") {
	case "hourly":
		return time.Hour, true
	case "daily":
		return 24 * time.Hour, true
	case "weekly":
		return 7 * 24 * time.Hour, true
	case "monthly":
		return 30 * 24 * time.Hour, true
	}
	return 0, false
}
