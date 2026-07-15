// cmd/profile-builder runs the profile store's batch expansion engine
// (pkg/profilebuilder): cluster identity_graph into persons, evaluate
// behavioural rules over the behaviour_signals lake table, expand
// memberships to every id in each person's cluster, and reconcile
// profile_signals into PG memberships.
//
// One-shot: run → log → exit; Kubernetes schedules the cadence (hourly
// CronJob, dayboundary pattern). Postgres is required; the lake and NATS
// degrade explicitly (no lake = clustering-only run, no NATS = warm caches
// pick changes up on their next interval poll).
package main

import (
	"context"
	"database/sql"
	"os"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events/natsbus"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/profilebuilder"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/datalake"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/fs"
	objs3 "github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/s3"
	_ "github.com/lib/pq"
)

func main() {
	log := logger.New("profile-builder")
	sc := config.Setup("profile-builder", nil, log)
	cfg := sc.Cfg

	db, err := sql.Open("postgres", keys.Database.URL.Get(cfg))
	if err == nil {
		err = db.Ping()
	}
	if err != nil {
		log.Error("postgres unavailable — profile-builder cannot run", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	// Lake: same object store the pipeline writes. Missing S3 config falls
	// back to the local filesystem root (offline dev), matching the pipeline.
	var lake datalake.Store
	bucket := keys.ProfileBuilder.DatalakeBucket.Get(cfg)
	if endpoint := cfg.Get(keys.S3.Endpoint.Key(), ""); endpoint != "" {
		obj, err := objs3.New(objs3.Config{
			Endpoint:  endpoint,
			AccessKey: keys.S3.AccessKey.Get(cfg),
			SecretKey: keys.S3.SecretKey.Get(cfg),
			Region:    keys.S3.Region.Get(cfg),
			UseSSL:    keys.S3.UseSSL.Get(cfg),
		})
		if err != nil {
			log.Error("s3 connect failed — running clustering only (no rules/reconcile)", "error", err)
		} else {
			lake = datalake.NewObjectStore(obj, bucket, log)
		}
	} else if obj, err := fs.New("/tmp/adtech-datalake"); err == nil {
		log.Warn("s3.endpoint not set — lake reads from local filesystem", "root", "/tmp/adtech-datalake")
		lake = datalake.NewObjectStore(obj, bucket, log)
	}

	var bus events.EventBus
	if b, err := natsbus.New(keys.ProfileBuilder.NATSURL.Get(cfg), "profile-builder", log); err != nil {
		log.Warn("nats unavailable — audience invalidates skipped (caches refresh on interval)", "error", err)
	} else {
		defer b.Close()
		bus = b
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	res, err := profilebuilder.Run(ctx, profilebuilder.Config{
		DB:             db,
		Lake:           lake,
		Bus:            bus,
		Log:            log,
		MinConfidence:  keys.ProfileBuilder.MinConfidence.Get(cfg),
		MaxClusterSize: keys.ProfileBuilder.MaxClusterSize.Get(cfg),
	})
	if err != nil {
		log.Error("profile-builder failed", "error", err)
		os.Exit(1)
	}
	log.Info("profile-builder finished",
		"clusters", res.Clusters, "enrolled", res.Enrolled, "pruned", res.Pruned,
		"expanded", res.Expanded, "reconciled", res.Reconciled)
}
