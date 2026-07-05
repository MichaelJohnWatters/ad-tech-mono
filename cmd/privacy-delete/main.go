// cmd/privacy-delete executes pending Level-3 (full deletion) privacy requests.
// Run as a periodic CronJob (or the Tilt manual resource): each invocation
// purges every user with a Level-3 opt_out_registry row that isn't yet
// completed — across identity_graph + audience_segment_members — marks the row
// completed, announces adtech.privacy.deletion_completed, then exits.
//
// One-shot: load pending → purge → mark → announce → exit. Kubernetes schedules
// the cadence.
package main

import (
	"context"
	"database/sql"
	"os"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events/natsbus"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/privacydelete"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	_ "github.com/lib/pq"
)

func main() {
	log := logger.New("privacy-delete")
	sc := config.Setup("privacy-delete", nil, log)
	cfg := sc.Cfg

	dbURL := cfg.Get("database.url", routes.DefaultPostgresURL)
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Error("open postgres", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	// NATS is optional — the deletion still happens without it; the completion
	// announcement just lets downstream caches refresh sub-poll.
	var announce privacydelete.Announcer
	if bus, err := natsbus.New(cfg.Get("privacy_delete.nats_url", routes.DefaultNATSURL), "privacy-delete", log); err != nil {
		log.Warn("nats unavailable — deletions proceed, completion not announced", "error", err)
	} else {
		defer bus.Close()
		announce = bus
	}

	deleter := &privacydelete.Deleter{
		Store:    privacydelete.NewPostgresStore(db),
		Announce: announce,
		Subject:  events.SubjectPrivacyCompleted,
		Log:      log,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	n, err := deleter.RunPending(ctx)
	if err != nil {
		log.Error("privacy-delete failed", "error", err)
		os.Exit(1)
	}
	log.Info("privacy-delete complete", "users_deleted", n)
}
