// cmd/report-runner executes due scheduled reports. Run as a periodic CronJob
// (or the Tilt manual resource): each invocation runs every saved report whose
// interval schedule (@hourly/@daily/@weekly/@monthly) is due and emails the
// result to the account owner, then exits.
//
// One-shot: load due → query reporting → deliver → mark run → exit. Kubernetes
// schedules the cadence.
package main

import (
	"context"
	"database/sql"
	"net/http"
	"os"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/email"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/reportrunner"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	_ "github.com/lib/pq"
)

func main() {
	log := logger.New("report-runner")
	sc := config.Setup("report-runner", nil, log)
	cfg := sc.Cfg

	dbURL := cfg.Get("database.url", routes.DefaultPostgresURL)
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Error("open postgres", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	reportingURL := cfg.Get("report_runner.reporting_url", routes.DefaultReportingURL)
	from := cfg.Get("report_runner.email_from", "reports@adtech.local")

	// SMTP (Mailpit/SES) when configured, else an in-memory sender that just
	// logs each delivery — keeps the runner runnable locally without Mailpit.
	var sender email.Sender
	if host := cfg.Get("report_runner.smtp_host", ""); host != "" {
		sender = email.NewSMTP(host, from, log)
		log.Info("report-runner email via SMTP", "host", host)
	} else {
		sender = email.NewMemory(log)
		log.Info("report-runner email via memory sender (no smtp_host set) — deliveries are logged only")
	}

	runner := &reportrunner.Runner{
		Store: reportrunner.NewPostgresStore(db),
		Query: reportrunner.HTTPQuery(reportingURL, &http.Client{Timeout: 30 * time.Second}),
		Email: sender,
		From:  from,
		Now:   time.Now,
		Log:   log,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	ran, err := runner.RunDue(ctx)
	if err != nil {
		log.Error("report runner failed", "error", err)
		os.Exit(1)
	}
	log.Info("report runner complete", "reports_run", ran)
}
