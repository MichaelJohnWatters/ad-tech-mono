// cmd/account-closeout finalizes account closures whose 30-day grace period has
// elapsed (PLAN Phase 11, item 105). For each due closure it generates a final
// invoice (advertisers), marks the account closed and closes the request. The
// 90-day data purge is DEFERRED — closed_at is left set so the future purge job
// can scan for it. Designed as a daily K8s CronJob (and host-runnable one-off);
// idempotent — safe to re-run (each closure flips grace→closed exactly once).
//
// Usage:
//
//	go run ./cmd/account-closeout            # close out every due grace closure
package main

import (
	"context"
	"database/sql"
	"os"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/accountlifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/invoicing"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	_ "github.com/lib/pq"
)

func main() {
	log := logger.New("account-closeout")

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

	co := &accountlifecycle.CloseOut{
		DB:       db,
		Invoices: invoicing.New(db),
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
}
