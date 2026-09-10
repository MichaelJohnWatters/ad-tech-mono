// cmd/account-closeout finalizes account closures whose 30-day grace period has
// elapsed (PLAN Phase 11, item 105). For each due closure it generates a final
// invoice (advertisers) or a final payout (publishers), marks the account closed
// and closes the request. The 90-day data purge is DEFERRED — closed_at is left
// set so the future purge job can scan for it. Designed as a daily K8s CronJob
// (and host-runnable one-off); idempotent — safe to re-run (each closure flips
// grace→closed exactly once).
//
// Usage:
//
//	go run ./cmd/account-closeout            # close out every due grace closure
package main

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/accountlifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/invoicing"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/payouts"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
	_ "github.com/lib/pq"
)

func main() {
	log := logger.New("account-closeout")

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = routes.DefaultPostgresURL
	}
	store, err := postgres.New(postgres.Config{PrimaryURL: dbURL, MaxOpenConns: 4, MaxIdleConns: 2, ConnMaxLifetime: 5 * time.Minute})
	if err != nil {
		log.Error("open db", "error", err)
		os.Exit(1)
	}
	defer store.Close()
	db := store.Primary()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		log.Error("ping db", "error", err)
		os.Exit(1)
	}

	// ClickHouse powers BOTH the publisher final-payout (gross source) and the
	// 90-day purge (analytics deletion). Built once; nil = degrade gracefully.
	ch := connectClickHouse(log)

	var payoutGen accountlifecycle.PayoutGenerator
	if ch != nil {
		payoutGen = payouts.New(store, ch)
	}
	co := &accountlifecycle.CloseOut{
		DB:       db,
		Invoices: invoicing.New(db),
		Payouts:  payoutGen,
		Now:      time.Now,
		Log:      log,
	}
	log.Info("account close-out starting")
	n, err := co.RunDue(ctx)
	if err != nil {
		log.Error("account close-out failed", "closed", n, "error", err)
		os.Exit(1)
	}
	log.Info("account close-out complete", "closed", n)

	// 90-day destructive purge of accounts closed past the retention window.
	var analyticsPurger accountlifecycle.AnalyticsPurger
	if ch != nil {
		analyticsPurger = ch
	}
	purger := &accountlifecycle.Purger{DB: db, Analytics: analyticsPurger, Now: time.Now, Log: log}
	purged, err := purger.RunDuePurges(ctx)
	if err != nil {
		log.Error("account purge failed", "purged", purged, "error", err)
		os.Exit(1)
	}
	log.Info("account purge complete", "purged", purged)
}

// connectClickHouse opens the analytics store used by BOTH the publisher
// final-payout (gross source) and the 90-day purge (analytics deletion). Degrades
// gracefully: if ClickHouse is unreachable it returns nil, so advertiser closeouts
// still settle, publisher payouts log a deferral, and the purge skips the
// ClickHouse half (leaving those accounts for a later run) instead of failing.
func connectClickHouse(log *slog.Logger) *analytics.ClickHouse {
	chAddr := os.Getenv("CLICKHOUSE_ADDR")
	if chAddr == "" {
		chAddr = "127.0.0.1:9000"
	}
	chDB := os.Getenv("CLICKHOUSE_DATABASE")
	if chDB == "" {
		chDB = "adtech"
	}
	chUser := os.Getenv("CLICKHOUSE_USER")
	if chUser == "" {
		chUser = "adtech"
	}
	chPass := os.Getenv("CLICKHOUSE_PASSWORD")
	if chPass == "" {
		chPass = "adtech-local-dev"
	}
	ch, err := analytics.NewClickHouse(analytics.ClickHouseConfig{
		Addrs: strings.Split(chAddr, ","), Database: chDB, Username: chUser, Password: chPass,
	})
	if err != nil {
		log.Warn("clickhouse unavailable — publisher payouts + analytics purge will be skipped this run", "addr", chAddr, "error", err)
		return nil
	}
	return ch
}
