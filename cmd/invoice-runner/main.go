// cmd/invoice-runner generates advertiser invoices from billed spend. Designed
// as a monthly K8s CronJob (and host-runnable one-off): it sums
// campaign_committed_spend.settled_micros per campaign over a period, converts
// micros → dollars, and writes one invoices row + per-campaign line items per
// advertiser account. Idempotent — safe to re-run (invoices are keyed on
// account + period; a re-run refreshes the total and line items).
//
// Usage:
//
//	go run ./cmd/invoice-runner                       # last calendar month, all accounts
//	go run ./cmd/invoice-runner --month 2026-06       # a specific month, all accounts
//	go run ./cmd/invoice-runner --account <uuid>      # last month, one account
//	go run ./cmd/invoice-runner --period-start 2026-06-01 --period-end 2026-07-01
package main

import (
	"context"
	"database/sql"
	"flag"
	"os"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/invoicing"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	_ "github.com/lib/pq"
)

func main() {
	account := flag.String("account", "", "generate for a single advertiser account UUID (default: all accounts with spend)")
	month := flag.String("month", "", "month to bill as YYYY-MM (overrides the default last-calendar-month); ignored if --period-start/--period-end are set")
	periodStartStr := flag.String("period-start", "", "period start date YYYY-MM-DD (inclusive); requires --period-end")
	periodEndStr := flag.String("period-end", "", "period end date YYYY-MM-DD (exclusive); requires --period-start")
	flag.Parse()

	log := logger.New("invoice-runner")

	periodStart, periodEnd, err := resolvePeriod(*month, *periodStartStr, *periodEndStr, time.Now())
	if err != nil {
		log.Error("invalid period", "error", err)
		os.Exit(1)
	}

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = routes.DefaultPostgresURL
	}
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Error("open db", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		log.Error("ping db", "error", err)
		os.Exit(1)
	}

	gen := invoicing.New(db)
	log.Info("invoice run starting",
		"period_start", periodStart.Format("2006-01-02"),
		"period_end", periodEnd.Format("2006-01-02"),
		"scope", scopeLabel(*account))

	if *account != "" {
		id, err := gen.GenerateForAccount(ctx, *account, periodStart, periodEnd)
		if err != nil {
			log.Error("invoice generation failed", "account_id", *account, "error", err)
			os.Exit(1)
		}
		if id == "" {
			log.Info("no spend in period; no invoice written", "account_id", *account)
		} else {
			log.Info("invoice generated", "account_id", *account, "invoice_id", id)
		}
		return
	}

	ids, err := gen.GenerateForAllAccounts(ctx, periodStart, periodEnd)
	if err != nil {
		log.Error("invoice generation failed", "invoices_written", len(ids), "error", err)
		os.Exit(1)
	}
	log.Info("invoice run complete", "invoices_written", len(ids))
}

// resolvePeriod picks the [start, end) invoice window from the flags, defaulting
// to the calendar month preceding now. Precedence: explicit --period-start/-end
// > --month > default last month.
func resolvePeriod(month, startStr, endStr string, now time.Time) (start, end time.Time, err error) {
	if startStr != "" || endStr != "" {
		if start, err = time.Parse("2006-01-02", startStr); err != nil {
			return start, end, err
		}
		if end, err = time.Parse("2006-01-02", endStr); err != nil {
			return start, end, err
		}
		return start.UTC(), end.UTC(), nil
	}
	if month != "" {
		start, err = time.Parse("2006-01", month)
		if err != nil {
			return start, end, err
		}
		start = start.UTC()
		return start, start.AddDate(0, 1, 0), nil
	}
	start, end = invoicing.LastCalendarMonth(now)
	return start, end, nil
}

func scopeLabel(account string) string {
	if account != "" {
		return "account:" + account
	}
	return "all-accounts"
}
