// cmd/report-runner is the async report worker. It runs three loops:
//
//   - scheduler tick: enqueues due saved reports (interval schedules
//     @hourly/@daily/@weekly/@monthly) as report jobs, resolving tenant scope
//     filters at enqueue time (pkg/reportjobs.ResolveTenantFilters).
//   - executor tick: claims queued jobs (Postgres FOR UPDATE SKIP LOCKED),
//     runs the query against reporting, renders the CSV/JSON/Parquet artifact
//     into the private reports bucket, and emails a download link when the
//     job's delivery is email.
//   - sweep tick: deletes expired artifacts + job rows (retention).
//
// Jobs also arrive from the gateway (POST /v1/api/reports/jobs) — the queue is
// the report_jobs table, whoever enqueues.
package main

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/accountexport"
	accountexportpg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/accountexport/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/email"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events/natsbus"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/health"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/reportjobs"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/reportrunner"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
	_ "github.com/lib/pq"
)

func main() {
	log := logger.New(constants.ServiceReportRunner)
	sc := config.Setup(constants.ServiceReportRunner, keys.ReportRunnerSchema(), log)
	cfg := sc.Cfg
	hlth := health.New()
	lc := lifecycle.New(log)

	otelShutdown := tracing.Init(context.Background(), tracing.Config{
		ServiceName:    constants.ServiceReportRunner,
		ServiceVersion: keys.Otel.ServiceVersion.Get(cfg),
		Endpoint:       keys.Otel.Endpoint.Get(cfg),
		SampleRatio:    keys.Otel.SampleRatio.Get(cfg),
		Log:            log,
	})
	lc.OnShutdown("otel", func(ctx context.Context) error { return otelShutdown(ctx) })

	port := keys.ReportRunner.Port.Get(cfg)

	// Postgres — the job queue + schedule source. Fail-soft on boot: readiness
	// pings the DB rather than crash-looping before it's reachable.
	dbURL := keys.Database.URL.Get(cfg)
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Error("open postgres", "error", err)
	}
	if db != nil {
		db.SetMaxOpenConns(4)
		db.SetMaxIdleConns(2)
		db.SetConnMaxLifetime(5 * time.Minute)
		lc.OnShutdown("postgres", func(_ context.Context) error { return db.Close() })
		hlth.AddReadinessCheck("postgres", func(ctx context.Context) error { return db.PingContext(ctx) })
	}

	// Artifact store (Minio/S3, FS fallback). The bucket is PRIVATE — downloads
	// stream through the gateway after auth; never SetPublicRead here.
	objStore := objects.Connect(cfg, "/tmp/adtech-reports", log)
	bucket := keys.ReportRunner.ArtifactBucket.Get(cfg)
	if objStore != nil {
		if err := objStore.EnsureBucket(context.Background(), bucket); err != nil {
			// Self-heal, don't latch: a pod that boots racing Minio used to
			// ERROR once and then fail every job with "store artifact: The
			// specified bucket does not exist" until manually restarted
			// (bit the fresh stack on 2026-07-26 — four e2e suites red).
			log.Error("ensure artifact bucket failed; retrying in background", "bucket", bucket, "error", err)
			go func() {
				for {
					time.Sleep(15 * time.Second)
					if err := objStore.EnsureBucket(context.Background(), bucket); err == nil {
						log.Info("artifact bucket ensured after retry", "bucket", bucket)
						return
					}
				}
			}()
		}
	}

	from := keys.ReportRunner.EmailFrom.Get(cfg)
	sender := connectEmail(cfg, from, log)

	jobStore := reportjobs.NewPostgresJobStore(db)
	// POD_NAME is a per-DEPLOYMENT identity in this chart (all replicas
	// share it); the hostname is the actual pod. Both, for observability.
	if hn, _ := os.Hostname(); hn != "" {
		jobStore.WorkerID = hn
	} else {
		jobStore.WorkerID = os.Getenv("POD_NAME")
	}
	scope := reportjobs.PostgresScopeLookup{DB: db}
	retention := keys.ReportRunner.Retention.Get(cfg)

	executor := &reportjobs.Executor{
		Store: jobStore,
		Query: reportrunner.HTTPQuery(
			keys.ReportRunner.ReportingURL.Get(cfg),
			&http.Client{Timeout: keys.ReportRunner.QueryTimeout.Get(cfg)}),
		Objects:      objStore,
		Bucket:       bucket,
		Email:        sender,
		From:         from,
		GatewayURL:   keys.ReportRunner.PublicGatewayURL.Get(cfg),
		QueryTimeout: keys.ReportRunner.QueryTimeout.Get(cfg),
		Now:          time.Now,
		Log:          log,
		// Segment exports (profile store): rows come straight from the
		// memberships table rather than the reporting query API.
		SegmentMembers: reportjobs.PGSegmentMembers(db),
	}
	// report.completed announcements → webhooks dispatcher (delivery=webhook
	// and any subscribed account). Optional: NATS down = no announcements,
	// jobs still complete and download links still work.
	if bus, err := natsbus.New(keys.ReportRunner.NATSURL.Get(cfg), constants.ServiceReportRunner+"-events", log); err != nil {
		log.Warn("nats unavailable — report.completed announcements disabled", "error", err)
	} else {
		bus.EnsureStreamWithRetry(context.Background(), events.StreamName, []string{events.StreamSubjects})
		executor.Events = events.NewPublisher(bus, log)
		lc.OnShutdown("report-events", func(_ context.Context) error { return bus.Close() })
	}

	// Scheduler: due saved reports become schedule-sourced jobs. Tenant scope is
	// resolved here — the executor trusts the filters snapshotted on the job.
	runner := &reportrunner.Runner{
		Store: reportrunner.NewPostgresStore(db),
		Enqueue: func(ctx context.Context, rep reportrunner.ScheduledReport, now time.Time) error {
			filters, err := reportjobs.ResolveTenantFilters(ctx, scope, rep.AccountID, rep.QueryConfig.Filters)
			if err != nil {
				return err
			}
			params := rep.QueryConfig
			params.Filters = filters
			delivery := rep.Delivery
			// email + webhook both pass through: the executor emails directly and
			// announces report.completed for the webhook dispatcher. Anything else
			// (legacy/blank) normalises to none.
			if delivery != reportjobs.DeliveryEmail && delivery != reportjobs.DeliveryWebhook {
				delivery = reportjobs.DeliveryNone
			}
			format := rep.Format
			if !reportjobs.ValidFormat(format) {
				format = reportjobs.FormatCSV
			}
			_, err = jobStore.Enqueue(ctx, reportjobs.Job{
				AccountID:     rep.AccountID,
				SavedReportID: rep.ID,
				Name:          rep.Name,
				QueryConfig:   params,
				Format:        format,
				Delivery:      delivery,
				Recipient:     rep.Recipient,
				Source:        reportjobs.SourceSchedule,
				ExpiresAt:     now.Add(retention),
			})
			return err
		},
		Now: time.Now,
		Log: log,
	}

	sweeper := &reportjobs.Sweeper{Store: jobStore, Objects: objStore, Now: time.Now, Log: log}

	// Account data-export worker (PLAN Phase 11, item 105): drains
	// account_export_jobs, zipping each account's data into the same private
	// artifact bucket the report jobs use. Shares this service's object store.
	exportWorker := &accountexport.Worker{
		Store:   accountexportpg.New(db),
		Builder: &accountexport.Builder{DB: db, Objects: objStore, Bucket: bucket, Now: time.Now},
		Log:     log,
	}

	// Crash recovery: reclaim jobs whose LEASE lapsed (claimant stopped
	// heartbeating = genuinely dead). Lease-based, so this is safe with any
	// number of worker replicas — a booting pod can no longer steal jobs a
	// live peer is executing (the old started_at requeue could, which is
	// what pinned this service to 1 replica). Runs at boot and every tick.
	if n, err := jobStore.ReclaimExpired(context.Background()); err != nil {
		log.Warn("reclaim expired jobs", "error", err)
	} else if n > 0 {
		log.Info("reclaimed expired report jobs", "count", n)
	}

	loopCtx, stopLoops := context.WithCancel(context.Background())
	lc.OnShutdown("loops", func(_ context.Context) error { stopLoops(); return nil })
	go tickLoop(loopCtx, keys.ReportRunner.ScheduleInterval.Get(cfg), func(ctx context.Context) {
		if n, err := runner.RunDue(ctx); err != nil {
			log.Error("scheduler tick failed", "error", err)
		} else if n > 0 {
			log.Info("scheduler tick enqueued", "reports", n)
		}
	})
	go tickLoop(loopCtx, keys.ReportRunner.PollInterval.Get(cfg), func(ctx context.Context) {
		// Reclaim lapsed leases each tick — dead-peer recovery without
		// waiting for a worker restart.
		if n, err := jobStore.ReclaimExpired(ctx); err == nil && n > 0 {
			log.Info("reclaimed expired report jobs", "count", n)
		}
		for { // drain the queue each tick
			claimed, err := executor.RunOnce(ctx)
			if err != nil {
				log.Error("executor tick failed", "error", err)
				break
			}
			if !claimed || ctx.Err() != nil {
				break
			}
		}
		// Drain the account-export queue on the same tick.
		for {
			claimed, err := exportWorker.RunOnce(ctx)
			if err != nil {
				log.Error("account-export tick failed", "error", err)
				return
			}
			if !claimed || ctx.Err() != nil {
				return
			}
		}
	})
	go tickLoop(loopCtx, keys.ReportRunner.SweepInterval.Get(cfg), func(ctx context.Context) {
		if _, err := sweeper.SweepOnce(ctx); err != nil {
			log.Error("sweep tick failed", "error", err)
		}
	})

	metrics := middleware.NewMetrics(constants.ServiceReportRunner)
	mux := http.NewServeMux()
	mux.Handle(routes.Healthz, hlth.LivenessHandler())
	mux.Handle(routes.Readyz, hlth.ReadinessHandler())
	mux.Handle(routes.Metrics, metrics.Handler())

	handler := tracing.HTTPMiddleware(constants.ServiceReportRunner)(metrics.Wrap(middleware.CORS(mux)))
	server := &http.Server{Addr: ":" + port, Handler: handler, ReadTimeout: 5 * time.Second, WriteTimeout: 30 * time.Second}

	log.Info("report-runner starting", "port", port, "bucket", bucket)
	lifecycle.ServeHTTP(lc, server, log, 30*time.Second)
}

// tickLoop runs fn immediately and then on every tick until ctx is cancelled.
func tickLoop(ctx context.Context, interval time.Duration, fn func(context.Context)) {
	fn(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn(ctx)
		}
	}
}

// connectEmail selects SMTP (Mailpit/SES) when configured, else an in-memory
// sender that only logs deliveries.
func connectEmail(cfg *config.Config, from string, log *slog.Logger) email.Sender {
	host := keys.ReportRunner.SMTPHost.Get(cfg)
	if host == "" {
		log.Info("report-runner email via memory sender (no smtp_host set) — deliveries are logged only")
		return email.NewMemory(log)
	}
	if user := keys.ReportRunner.SMTPUsername.Get(cfg); user != "" {
		log.Info("report-runner email via authenticated SMTP", "host", host, "username", user)
		return email.NewSMTPAuth(host, from, user, keys.ReportRunner.SMTPPassword.Get(cfg), log)
	}
	log.Info("report-runner email via SMTP", "host", host)
	return email.NewSMTP(host, from, log)
}
