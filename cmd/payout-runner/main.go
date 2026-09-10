// payout-runner generates publisher payouts for a period — the money-OUT mirror
// of invoice-runner. For each active publisher it sums the period's gross revenue
// from the analytics store (ClickHouse), applies the publisher's revenue-share
// contract to get the net owed, gates on the publisher's minimum-payout
// threshold, and writes one idempotent `payouts` row per (publisher, period).
//
// Runs monthly as a CronJob (after invoice-runner) and is host-runnable as a
// one-off: `go run ./cmd/payout-runner` (defaults to last calendar month, all
// publishers). Flags mirror invoice-runner: --publisher, --month, or an explicit
// --period-start/--period-end window.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/invoicing"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/payouts"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

func main() {
	publisher := flag.String("publisher", "", "generate for a single publisher UUID (default: all active publishers)")
	month := flag.String("month", "", "month to pay out as YYYY-MM (overrides the default last-calendar-month); ignored if --period-start/--period-end are set")
	periodStartStr := flag.String("period-start", "", "period start date YYYY-MM-DD (inclusive); requires --period-end")
	periodEndStr := flag.String("period-end", "", "period end date YYYY-MM-DD (exclusive); requires --period-start")
	flag.Parse()

	log := logger.New("payout-runner")

	periodStart, periodEnd, err := resolvePeriod(*month, *periodStartStr, *periodEndStr, time.Now())
	if err != nil {
		log.Error("invalid period", "error", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Postgres: contracts, payout methods, the payouts write (RLS hatches).
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = routes.DefaultPostgresURL
	}
	store, err := postgres.New(postgres.Config{PrimaryURL: dbURL, MaxOpenConns: 4, MaxIdleConns: 2, ConnMaxLifetime: 5 * time.Minute})
	if err != nil {
		log.Error("open postgres", "error", err)
		os.Exit(1)
	}
	defer store.Close()

	// ClickHouse: gross revenue per publisher.
	chAddr := os.Getenv("CLICKHOUSE_ADDR")
	if chAddr == "" {
		chAddr = "127.0.0.1:9000"
	}
	chDB := os.Getenv("CLICKHOUSE_DATABASE")
	if chDB == "" {
		chDB = "adtech"
	}
	// Mirror reporting's ClickHouse credential defaults (reporting.clickhouse_user
	// / _password). Prod overlays inject these from a K8s Secret via env.
	chUser := os.Getenv("CLICKHOUSE_USER")
	if chUser == "" {
		chUser = "adtech"
	}
	chPass := os.Getenv("CLICKHOUSE_PASSWORD")
	if chPass == "" {
		chPass = "adtech-local-dev"
	}
	ch, err := analytics.NewClickHouse(analytics.ClickHouseConfig{
		Addrs:    strings.Split(chAddr, ","),
		Database: chDB,
		Username: chUser,
		Password: chPass,
	})
	if err != nil {
		log.Error("open clickhouse", "addr", chAddr, "error", err)
		os.Exit(1)
	}
	defer ch.Close()

	gen := payouts.New(store, ch)
	log.Info("payout run starting",
		"period_start", periodStart.Format("2006-01-02"),
		"period_end", periodEnd.Format("2006-01-02"),
		"scope", scopeLabel(*publisher))

	if *publisher != "" {
		written, err := gen.GenerateForPublisher(ctx, *publisher, periodStart, periodEnd)
		if err != nil {
			log.Error("payout generation failed", "publisher_id", *publisher, "error", err)
			os.Exit(1)
		}
		if !written {
			log.Info("no payout written (no revenue or below minimum threshold)", "publisher_id", *publisher)
		} else {
			log.Info("payout generated", "publisher_id", *publisher)
		}
		return
	}

	res, err := gen.GenerateForAllPublishers(ctx, periodStart, periodEnd)
	if err != nil {
		log.Error("payout generation failed", "written", res.Written, "error", err)
		os.Exit(1)
	}
	log.Info("payout run complete", "written", res.Written, "unchanged_already_paid", res.Unchanged, "held_below_minimum", res.HeldLow, "zero_revenue", res.ZeroRev)
}

// resolvePeriod picks the [start, end) payout window from the flags, defaulting
// to the calendar month preceding now. Precedence: explicit --period-start/-end
// > --month > last-calendar-month. Mirrors invoice-runner exactly.
func resolvePeriod(month, startStr, endStr string, now time.Time) (start, end time.Time, err error) {
	if startStr != "" || endStr != "" {
		if startStr == "" || endStr == "" {
			return start, end, fmt.Errorf("--period-start and --period-end must be set together")
		}
		if start, err = time.Parse("2006-01-02", startStr); err != nil {
			return start, end, fmt.Errorf("bad --period-start: %w", err)
		}
		if end, err = time.Parse("2006-01-02", endStr); err != nil {
			return start, end, fmt.Errorf("bad --period-end: %w", err)
		}
		if !end.After(start) {
			return start, end, fmt.Errorf("--period-end must be after --period-start")
		}
		return start, end, nil
	}
	if month != "" {
		if start, err = time.Parse("2006-01", month); err != nil {
			return start, end, fmt.Errorf("bad --month (want YYYY-MM): %w", err)
		}
		return start, start.AddDate(0, 1, 0), nil
	}
	start, end = invoicing.LastCalendarMonth(now)
	return start, end, nil
}

func scopeLabel(publisher string) string {
	if publisher != "" {
		return "publisher=" + publisher
	}
	return "all-publishers"
}
