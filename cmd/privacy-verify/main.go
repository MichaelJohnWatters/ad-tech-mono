// cmd/privacy-verify audits completed Level-3 deletions. Run as a periodic
// CronJob (or the Tilt manual resource) after cmd/privacy-delete: each
// invocation re-checks every completed-but-unverified user for residual rows in
// identity_graph / audience_segment_members, stamps verified_at when clean, and
// logs an ERROR (leaving the row unverified) when data survived — the signal a
// deletion didn't fully propagate.
//
// One-shot: load unverified → residual check → stamp/flag → exit.
package main

import (
	"context"
	"database/sql"
	"os"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/privacydelete"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	_ "github.com/lib/pq"
)

func main() {
	log := logger.New("privacy-verify")
	sc := config.Setup("privacy-verify", nil, log)
	cfg := sc.Cfg

	dbURL := cfg.Get("database.url", routes.DefaultPostgresURL)
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Error("open postgres", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	verifier := &privacydelete.Verifier{
		Store: privacydelete.NewPostgresStore(db),
		Log:   log,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	verified, incomplete, err := verifier.RunUnverified(ctx)
	if err != nil {
		log.Error("privacy-verify failed", "error", err)
		os.Exit(1)
	}
	log.Info("privacy-verify complete", "verified", verified, "incomplete", incomplete)
	// A non-zero incomplete count is an operational alert (residual PII after a
	// deletion) — exit non-zero so a CronJob surfaces it as a failed run.
	if incomplete > 0 {
		os.Exit(2)
	}
}
