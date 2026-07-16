// cmd/webhooks is the webhook dispatcher. It consumes account-scoped business
// events from NATS JetStream, looks up each account's active webhook
// subscriptions for that event type, and delivers an HMAC-signed JSON envelope
// to every registered endpoint (with retries + a persisted delivery log).
//
// Customers register endpoints via the gateway CRUD API (POST /v1/api/webhooks)
// and subscribe to event types by name — the same names this service maps NATS
// subjects onto (see eventRoutes below).
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events/natsbus"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/health"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/webhooks"
	_ "github.com/lib/pq"
)

// eventRoutes maps a NATS subject to the customer-facing webhook event-type
// name (the string an account puts in a subscription's `events` list). Adding a
// new webhook event = one entry here + documenting it in PLAN.md's Webhook
// Subjects table. Every mapped event payload carries an `account_id`.
var eventRoutes = map[string]string{
	events.SubjectBudgetDepleted:       "budget.depleted",
	events.SubjectBalanceDepleted:      "balance.depleted",
	events.SubjectCampaignStateChanged: "campaign.state_changed",
	events.SubjectReportCompleted:      "report.completed",
}

func main() {
	log := logger.New(constants.ServiceWebhooks)
	sc := config.Setup(constants.ServiceWebhooks, keys.WebhooksSchema(), log)
	cfg := sc.Cfg
	hlth := health.New()
	lc := lifecycle.New(log)

	// OpenTelemetry — empty endpoint disables tracing so dev/test envs without
	// Jaeger still boot. The consumer span stitches to the publisher's trace via
	// the traceparent NATS header (natsbus extracts it).
	otelShutdown := tracing.Init(context.Background(), tracing.Config{
		ServiceName:    constants.ServiceWebhooks,
		ServiceVersion: keys.Otel.ServiceVersion.Get(cfg),
		Endpoint:       keys.Otel.Endpoint.Get(cfg),
		SampleRatio:    keys.Otel.SampleRatio.Get(cfg),
		Log:            log,
	})
	lc.OnShutdown("otel", func(ctx context.Context) error { return otelShutdown(ctx) })

	port := keys.Webhooks.Port.Get(cfg)

	// Postgres — the subscription source + delivery log. Fail-soft on boot: a
	// pod that starts before Postgres is reachable stays un-ready (readiness
	// pings the DB) rather than crash-looping.
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

	dispatcher := &webhooks.Dispatcher{
		Store:       webhooks.NewPostgresStore(db),
		HTTP:        &http.Client{Timeout: cfg.GetDuration(keys.Webhooks.HttpTimeout.Key(), 5*time.Second)},
		MaxAttempts: keys.Webhooks.MaxAttempts.Get(cfg),
		Backoff: func(attempt int) time.Duration {
			base := keys.Webhooks.BackoffBase.Get(cfg)
			return time.Duration(1<<uint(attempt-1)) * base
		},
		Now: time.Now,
		Log: log,
	}

	// NATS — the event source. Without it the dispatcher can't do its job, so
	// readiness fails when NATS is unavailable (unlike reporting, which has an
	// HTTP-ingest fallback).
	natsURL := keys.Webhooks.NATSURL.Get(cfg)
	natsBus, err := natsbus.New(natsURL, constants.ServiceWebhooks, log)
	if err != nil {
		log.Error("nats unavailable — no events will be delivered until it recovers", "error", err)
		hlth.AddReadinessCheck("nats", func(context.Context) error { return err })
	} else {
		lc.OnShutdown("nats", func(_ context.Context) error { return natsBus.Close() })
		ctx := context.Background()
		if serr := natsBus.EnsureStream(ctx, events.StreamName, []string{events.StreamSubjects}); serr != nil {
			log.Warn("ensure stream", "error", serr)
		}
		for subject, eventType := range eventRoutes {
			if serr := natsBus.Subscribe(ctx, subject, constants.NATSGroupWebhooks, eventHandler(dispatcher, eventType, log)); serr != nil {
				log.Error("subscribe failed", "subject", subject, "error", serr)
			}
		}
		log.Info("webhooks dispatcher consuming events", "subjects", len(eventRoutes))
	}

	metrics := middleware.NewMetrics(constants.ServiceWebhooks)
	mux := http.NewServeMux()
	mux.Handle(routes.Healthz, hlth.LivenessHandler())
	mux.Handle(routes.Readyz, hlth.ReadinessHandler())
	mux.Handle(routes.Metrics, metrics.Handler())

	handler := tracing.HTTPMiddleware(constants.ServiceWebhooks)(metrics.Wrap(middleware.CORS(mux)))
	server := &http.Server{Addr: ":" + port, Handler: handler, ReadTimeout: 5 * time.Second, WriteTimeout: 30 * time.Second}

	log.Info("webhooks starting", "port", port)
	lifecycle.ServeHTTP(lc, server, log, 30*time.Second)
}

// eventHandler routes one NATS message: extract the account, dispatch to the
// account's subscriptions for eventType, then ack. A malformed payload or a
// missing account is a poison message (ack — no redelivery); a subscription
// lookup failure is transient (nak — redeliver). Per-endpoint HTTP failures are
// retried and recorded inside Dispatch, so a delivered-but-failing receiver
// still acks the NATS message (we don't want NATS to re-fire an event we've
// already attempted with our own retry budget).
func eventHandler(d *webhooks.Dispatcher, eventType string, log *slog.Logger) events.Handler {
	return func(ctx context.Context, msg *events.Message) error {
		var route struct {
			AccountID string `json:"account_id"`
		}
		if err := json.Unmarshal(msg.Data, &route); err != nil || route.AccountID == "" {
			log.Warn("dropping webhook event with no account_id", "event", eventType, "error", err)
			return msg.Ack()
		}
		if err := d.Dispatch(ctx, webhooks.Event{
			Type:      eventType,
			AccountID: route.AccountID,
			Data:      json.RawMessage(msg.Data),
		}); err != nil {
			log.Error("webhook dispatch failed, will redeliver", "event", eventType, "account_id", route.AccountID, "error", err)
			return msg.Nak()
		}
		return msg.Ack()
	}
}
