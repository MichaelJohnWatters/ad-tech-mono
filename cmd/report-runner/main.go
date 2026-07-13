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
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/email"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/health"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/reportjobs"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/reportrunner"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/fs"
	objs3 "github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/s3"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
	_ "github.com/lib/pq"
)

func main() {
	log := logger.New(constants.ServiceReportRunner)
	sc := config.Setup(constants.ServiceReportRunner, nil, log)
	cfg := sc.Cfg
	hlth := health.New()
	lc := lifecycle.New(log)

	otelShutdown := tracing.Init(context.Background(), tracing.Config{
		ServiceName:    constants.ServiceReportRunner,
		ServiceVersion: cfg.Get("otel.service_version", "dev"),
		Endpoint:       cfg.Get("otel.endpoint", "localhost:4318"),
		SampleRatio:    cfg.GetFloat("otel.sample_ratio", 1.0),
		Log:            log,
	})
	lc.OnShutdown("otel", func(ctx context.Context) error { return otelShutdown(ctx) })

	port := cfg.Get("report_runner.port", routes.PortReportRunner)

	// Postgres — the job queue + schedule source. Fail-soft on boot: readiness
	// pings the DB rather than crash-looping before it's reachable.
	dbURL := cfg.Get("database.url", routes.DefaultPostgresURL)
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
	objStore := connectObjects(cfg, log)
	bucket := cfg.Get("report_runner.artifact_bucket", "adtech-reports")
	if objStore != nil {
		if err := objStore.EnsureBucket(context.Background(), bucket); err != nil {
			log.Error("ensure artifact bucket", "bucket", bucket, "error", err)
		}
	}

	from := cfg.Get("report_runner.email_from", "reports@adtech.local")
	sender := connectEmail(cfg, from, log)

	jobStore := reportjobs.NewPostgresJobStore(db)
	scope := reportjobs.PostgresScopeLookup{DB: db}
	retention := cfg.GetDuration("report_runner.retention", 720*time.Hour)

	executor := &reportjobs.Executor{
		Store: jobStore,
		Query: reportrunner.HTTPQuery(
			cfg.Get("report_runner.reporting_url", routes.DefaultReportingURL),
			&http.Client{Timeout: cfg.GetDuration("report_runner.query_timeout", 10*time.Minute)}),
		Objects:      objStore,
		Bucket:       bucket,
		Email:        sender,
		From:         from,
		GatewayURL:   cfg.Get("report_runner.public_gateway_url", routes.DefaultGatewayURL),
		QueryTimeout: cfg.GetDuration("report_runner.query_timeout", 10*time.Minute),
		Now:          time.Now,
		Log:          log,
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
			if delivery != reportjobs.DeliveryEmail {
				delivery = reportjobs.DeliveryNone // webhook delivery not built yet
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

	// Crash recovery: jobs stuck running from a previous worker are requeued.
	if n, err := jobStore.RequeueStuck(context.Background(),
		cfg.GetDuration("report_runner.stuck_after", 30*time.Minute)); err != nil {
		log.Warn("requeue stuck jobs", "error", err)
	} else if n > 0 {
		log.Info("requeued stuck report jobs", "count", n)
	}

	loopCtx, stopLoops := context.WithCancel(context.Background())
	lc.OnShutdown("loops", func(_ context.Context) error { stopLoops(); return nil })
	go tickLoop(loopCtx, cfg.GetDuration("report_runner.schedule_interval", time.Minute), func(ctx context.Context) {
		if n, err := runner.RunDue(ctx); err != nil {
			log.Error("scheduler tick failed", "error", err)
		} else if n > 0 {
			log.Info("scheduler tick enqueued", "reports", n)
		}
	})
	go tickLoop(loopCtx, cfg.GetDuration("report_runner.poll_interval", 5*time.Second), func(ctx context.Context) {
		for { // drain the queue each tick
			claimed, err := executor.RunOnce(ctx)
			if err != nil {
				log.Error("executor tick failed", "error", err)
				return
			}
			if !claimed || ctx.Err() != nil {
				return
			}
		}
	})
	go tickLoop(loopCtx, cfg.GetDuration("report_runner.sweep_interval", time.Hour), func(ctx context.Context) {
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

// connectObjects returns an S3/Minio store if configured, else a local FS
// store (mirrors the adserver wiring).
func connectObjects(cfg *config.Config, log *slog.Logger) objects.Store {
	endpoint := cfg.Get("s3.endpoint", "")
	if endpoint == "" {
		root := "/tmp/adtech-reports"
		log.Warn("s3.endpoint not set, using local filesystem", "root", root)
		store, err := fs.New(root)
		if err != nil {
			log.Error("fs store init failed", "error", err)
		}
		return store
	}
	store, err := objs3.New(objs3.Config{
		Endpoint:  endpoint,
		AccessKey: cfg.Get("s3.access_key", "adtech"),
		SecretKey: cfg.Get("s3.secret_key", "adtech-local-dev"),
		Region:    cfg.Get("s3.region", "us-east-1"),
		UseSSL:    cfg.GetBool("s3.use_ssl", false),
	})
	if err != nil {
		log.Error("s3 init failed, falling back to filesystem", "error", err)
		fsStore, _ := fs.New("/tmp/adtech-reports")
		return fsStore
	}
	log.Info("s3 connected", "endpoint", endpoint)
	return store
}

// connectEmail selects SMTP (Mailpit/SES) when configured, else an in-memory
// sender that only logs deliveries.
func connectEmail(cfg *config.Config, from string, log *slog.Logger) email.Sender {
	host := cfg.Get("report_runner.smtp_host", "")
	if host == "" {
		log.Info("report-runner email via memory sender (no smtp_host set) — deliveries are logged only")
		return email.NewMemory(log)
	}
	if user := cfg.Get("report_runner.smtp_username", ""); user != "" {
		log.Info("report-runner email via authenticated SMTP", "host", host, "username", user)
		return email.NewSMTPAuth(host, from, user, cfg.Get("report_runner.smtp_password", ""), log)
	}
	log.Info("report-runner email via SMTP", "host", host)
	return email.NewSMTP(host, from, log)
}
