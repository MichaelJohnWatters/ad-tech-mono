// Package reportrunner turns due scheduled reports into report jobs.
//
// It is the runtime behind the saved_reports.schedule field: a report with an
// interval schedule (@hourly/@daily/@weekly/@monthly) is enqueued on its
// cadence as an async report job (pkg/reportjobs) — executed by the worker,
// stored as a downloadable artifact, and emailed as a link when the report's
// delivery is email. The report-runner service ticks RunDue periodically.
//
// Schedules come in two flavours:
//   - interval keywords (@hourly/@daily/@weekly/@monthly): "due" means
//     now − last_run ≥ the interval (enqueue-to-enqueue cadence — the
//     original model, semantics unchanged);
//   - full 5-field cron expressions ("30 6 * * 1"): "due" means a cron
//     activation has passed since last_run (calendar-aligned).
package reportrunner

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

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
// on a known schedule is due immediately. Interval keywords keep their
// original enqueue-to-enqueue semantics; 5-field cron expressions are
// calendar-aligned (due when an activation time has passed since last_run).
func Due(schedule string, lastRun *time.Time, now time.Time) bool {
	if interval, ok := scheduleInterval(schedule); ok {
		if lastRun == nil {
			return true
		}
		return now.Sub(*lastRun) >= interval
	}
	sched, err := cronParser.Parse(strings.TrimSpace(schedule))
	if err != nil {
		return false
	}
	if lastRun == nil {
		return true
	}
	return !sched.Next(*lastRun).After(now)
}

// cronParser accepts standard 5-field expressions (minute precision). The
// @-macros are handled by scheduleInterval FIRST so their historical
// interval semantics don't silently change to cron's calendar alignment.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// ValidSchedule reports whether s is an accepted schedule — an interval
// keyword or a parseable 5-field cron expression. The gateway validates
// saved-report schedules with this so junk gets a 400 instead of a report
// that silently never runs.
func ValidSchedule(s string) bool {
	if strings.TrimSpace(s) == "" {
		return false
	}
	if _, ok := scheduleInterval(s); ok {
		return true
	}
	_, err := cronParser.Parse(strings.TrimSpace(s))
	return err == nil
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
