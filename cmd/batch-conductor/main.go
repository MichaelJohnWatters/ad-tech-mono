// cmd/batch-conductor runs the platform's data chain as ONE explicit,
// completion-ordered sequence (pkg/batch.StandardChain):
//
//	checkpoint → compact → rollups (minute→monthly) → profile-builder
//	→ privacy-delete → privacy-verify
//
// It replaced the time-staggered CronJob lattice (compact :05,
// profile-builder :15, privacy jobs by convention): each step now starts
// when the previous one actually finishes, every step is recorded in
// batch_runs (staff "Batch runs" page), and a completion event announces
// the run on NATS. One-shot; Kubernetes schedules the cadence (hourly).
// Exits non-zero when a CRITICAL step failed — that's a run needing
// attention, not an optional-step hiccup.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/batch"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events/natsbus"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/privacydelete"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/profilebuilder"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/datalake"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/fs"
	objs3 "github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/s3"
	_ "github.com/lib/pq"
)

func main() {
	log := logger.New("batch-conductor")
	sc := config.Setup("batch-conductor", nil, log)
	cfg := sc.Cfg

	db, err := sql.Open("postgres", keys.Database.URL.Get(cfg))
	if err == nil {
		err = db.Ping()
	}
	if err != nil {
		log.Error("postgres unavailable — batch chain cannot run", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	// Lake: same store the pipeline writes (profile-builder step needs it).
	var lake datalake.Store
	bucket := keys.BatchConductor.DatalakeBucket.Get(cfg)
	if endpoint := cfg.Get(keys.S3.Endpoint.Key(), ""); endpoint != "" {
		obj, err := objs3.New(objs3.Config{
			Endpoint:  endpoint,
			AccessKey: keys.S3.AccessKey.Get(cfg),
			SecretKey: keys.S3.SecretKey.Get(cfg),
			Region:    keys.S3.Region.Get(cfg),
			UseSSL:    keys.S3.UseSSL.Get(cfg),
		})
		if err != nil {
			log.Error("s3 connect failed — profile-builder step will run clustering only", "error", err)
		} else {
			lake = datalake.NewObjectStore(obj, bucket, log)
		}
	} else if obj, err := fs.New("/tmp/adtech-datalake"); err == nil {
		log.Warn("s3.endpoint not set — lake reads from local filesystem", "root", "/tmp/adtech-datalake")
		lake = datalake.NewObjectStore(obj, bucket, log)
	}

	var bus events.EventBus
	if b, err := natsbus.New(keys.BatchConductor.NATSURL.Get(cfg), "batch-conductor", log); err != nil {
		log.Warn("nats unavailable — invalidates + run announcement skipped", "error", err)
	} else {
		defer b.Close()
		bus = b
	}

	// Behaviour querier (ADR 0006 phase 2): the profile-builder step evaluates
	// rules + reconcile via server-side ClickHouse GROUP BY instead of loading
	// whole lake partitions into Go (the OOM fix). Empty addr, or a ClickHouse
	// that won't connect, degrades to the lake reads (never blocks the run).
	var behaviour profilebuilder.BehaviourQuerier
	if addr := strings.TrimSpace(keys.BatchConductor.ClickHouseAddr.Get(cfg)); addr != "" {
		q, err := profilebuilder.NewCHBehaviourQuerier(profilebuilder.CHConfig{
			Addrs:    splitAndTrim(addr),
			Database: keys.BatchConductor.ClickHouseDatabase.Get(cfg),
			Username: keys.BatchConductor.ClickHouseUser.Get(cfg),
			Password: keys.BatchConductor.ClickHousePassword.Get(cfg),
		})
		if err != nil {
			log.Error("clickhouse unavailable — profile-builder step falls back to lake reads (ADR 0006 phase 2 OOM fix disabled)", "addr", addr, "error", err)
		} else {
			defer q.Close()
			behaviour = q
			log.Info("profile-builder: reading behaviour/profile signals from ClickHouse (ADR 0006 phase 2)", "addr", addr)
		}
	} else {
		log.Warn("batch_conductor.clickhouse_addr empty — profile-builder step uses lake reads (ADR 0006 phase 2 OOM fix disabled)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	deps := batch.Deps{
		DB:             db,
		Lake:           lake,
		Bus:            bus,
		Behaviour:      behaviour,
		PipelineURL:    keys.BatchConductor.PipelineURL.Get(cfg),
		ReportingURL:   keys.BatchConductor.ReportingURL.Get(cfg),
		Log:            log,
		MinConfidence:  keys.ProfileBuilder.MinConfidence.Get(cfg),
		MaxClusterSize: keys.ProfileBuilder.MaxClusterSize.Get(cfg),
		PrivacyExtras:  privacydelete.BuildExtras(cfg, log),
	}
	rec := &batch.Recorder{DB: db, Log: log}
	res, err := batch.RunChain(ctx, batch.StandardChain(deps), rec, log)

	if bus != nil {
		payload, _ := json.Marshal(map[string]any{
			"schema_version": 1, "run_id": res.RunID, "aborted": res.Aborted,
			"failed_steps": res.Failed, "duration_ms": res.Duration.Milliseconds(),
		})
		if perr := bus.Publish(ctx, events.SubjectBatchRunCompleted, payload); perr != nil {
			log.Warn("batch run announcement failed", "error", perr)
		}
	}

	log.Info("batch chain finished", "run_id", res.RunID,
		"steps", len(res.Steps), "failed", res.Failed, "aborted", res.Aborted,
		"duration_ms", res.Duration.Milliseconds())
	if err != nil {
		os.Exit(1)
	}
	if _, verifyFailed := findFailed(res, "privacy-verify"); verifyFailed {
		// Residual PII after a deletion — surface as a failed run (the old
		// standalone privacy-verify exited 2 for the same reason).
		os.Exit(2)
	}
}

// splitAndTrim splits a comma-separated list into trimmed, non-empty entries
// (ClickHouse host:port addresses).
func splitAndTrim(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// findFailed reports whether the named step failed.
func findFailed(res batch.ChainResult, name string) (batch.StepOutcome, bool) {
	for _, s := range res.Steps {
		if s.Step == name && s.Status == "failed" {
			return s, true
		}
	}
	return batch.StepOutcome{}, false
}
