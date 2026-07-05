// Package reportrunner executes due scheduled reports and delivers the results.
//
// It is the runtime behind the saved_reports.schedule field: a report with an
// interval schedule (@hourly/@daily/@weekly/@monthly) and email delivery is run
// on its cadence and emailed to the account owner. The runner is a one-shot —
// a CronJob (or the Tilt manual resource) schedules the cadence; each invocation
// runs every report that is currently due and exits.
//
// The schedule model is interval-based (not full cron) to avoid a parser
// dependency: "due" means now − last_run ≥ the schedule's interval.
package reportrunner

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/email"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

// ScheduledReport is a saved_reports row the runner may execute, joined with the
// recipient (account owner email) resolved from team_members.
type ScheduledReport struct {
	ID          string
	AccountID   string
	Name        string
	Schedule    string
	Delivery    string // email | webhook | none
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
// implementation posts to the reporting service (account_id injected for tenant
// scope); it is an interface so tests can fake it.
type QueryFunc func(ctx context.Context, accountID string, params analytics.QueryParams) (analytics.QueryResult, error)

// Runner executes due reports.
type Runner struct {
	Store Store
	Query QueryFunc
	Email email.Sender
	From  string // From address for delivered emails
	Now   func() time.Time
	Log   *slog.Logger
}

// RunDue runs every report that is currently due and returns how many ran.
// Individual failures are logged and skipped — one bad report never blocks the
// rest, and a failed run is NOT marked (so it retries next tick).
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
			r.Log.Error("scheduled report run failed", "report", rep.ID, "name", rep.Name, "error", err)
			continue
		}
		ran++
	}
	return ran, nil
}

func (r *Runner) runOne(ctx context.Context, rep ScheduledReport, now time.Time) error {
	// Tenant scope: force account_id so a report can only ever see its own data.
	params := rep.QueryConfig
	if params.Filters == nil {
		params.Filters = map[string]string{}
	}
	params.Filters["account_id"] = rep.AccountID

	result, err := r.Query(ctx, rep.AccountID, params)
	if err != nil {
		return fmt.Errorf("query: %w", err)
	}

	if rep.Delivery == "email" {
		if rep.Recipient == "" {
			return fmt.Errorf("no recipient email for account %s", rep.AccountID)
		}
		msg := email.Message{
			To:      rep.Recipient,
			From:    r.From,
			Subject: "Scheduled report: " + rep.Name,
			Body:    FormatResult(rep.Name, result, now),
		}
		if err := r.Email.Send(ctx, msg); err != nil {
			return fmt.Errorf("email: %w", err)
		}
	}

	if err := r.Store.MarkRun(ctx, rep.ID, now); err != nil {
		return fmt.Errorf("mark run: %w", err)
	}
	r.Log.Info("scheduled report delivered", "report", rep.ID, "name", rep.Name,
		"delivery", rep.Delivery, "rows", len(result.Rows))
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

// FormatResult renders a query result as a plain-text table for the email body.
func FormatResult(name string, res analytics.QueryResult, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Scheduled report: %s\nGenerated: %s\n\n", name, now.UTC().Format(time.RFC1123))
	if len(res.Columns) == 0 {
		b.WriteString("(no columns)\n")
		return b.String()
	}
	b.WriteString(strings.Join(res.Columns, "\t") + "\n")
	if len(res.Rows) == 0 {
		b.WriteString("(no rows in the reporting window)\n")
		return b.String()
	}
	for _, row := range res.Rows {
		cells := make([]string, len(row))
		for i, c := range row {
			cells[i] = fmt.Sprintf("%v", c)
		}
		b.WriteString(strings.Join(cells, "\t") + "\n")
	}
	fmt.Fprintf(&b, "\n%d row(s).\n", len(res.Rows))
	return b.String()
}
