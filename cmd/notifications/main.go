// cmd/notifications is the in-app notification builder. It consumes the same
// account-scoped business events the webhooks dispatcher does (budget/balance
// depleted, campaign state changed, report completed) and writes one durable
// per-account row into the `notifications` table for each. The gateway serves
// the portal bell + dropdown (list / unread-count / mark-read) from that table.
//
// It's the DB-row sibling of cmd/webhooks: same consumer shape, same subjects,
// but the sink is a Postgres insert instead of an HTTP POST. Run a SINGLE
// replica — the NATS group name (constants.NATSGroupNotifications) is
// queue-grouped so extra replicas would load-balance rather than duplicate, but
// one replica is plenty for this write volume and keeps ordering simple.
package main

import (
	"context"
	"database/sql"
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
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/notifications"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
	_ "github.com/lib/pq"
)

// notificationSubjects are the NATS subjects that map to a per-account
// notification. Every one carries an account_id (the notification's tenant
// owner); the pkg/notifications.Translate function does the payload → row
// mapping. Adding a notifiable event = one entry here + a case in Translate.
var notificationSubjects = []string{
	events.SubjectBudgetDepleted,
	events.SubjectBalanceDepleted,
	events.SubjectCampaignStateChanged,
	events.SubjectReportCompleted,
}

func main() {
	log := logger.New(constants.ServiceNotifications)
	sc := config.Setup(constants.ServiceNotifications, keys.NotificationsSchema(), log)
	cfg := sc.Cfg
	hlth := health.New()
	lc := lifecycle.New(log)

	// OpenTelemetry — empty endpoint disables tracing so dev/test envs without
	// Jaeger still boot. The consumer span stitches to the publisher's trace via
	// the traceparent NATS header (natsbus extracts it).
	otelShutdown := tracing.Init(context.Background(), tracing.Config{
		ServiceName:    constants.ServiceNotifications,
		ServiceVersion: keys.Otel.ServiceVersion.Get(cfg),
		Endpoint:       keys.Otel.Endpoint.Get(cfg),
		SampleRatio:    keys.Otel.SampleRatio.Get(cfg),
		Log:            log,
	})
	lc.OnShutdown("otel", func(ctx context.Context) error { return otelShutdown(ctx) })

	port := keys.Notifications.Port.Get(cfg)

	// Postgres — the notification sink. Fail-soft on boot: a pod that starts
	// before Postgres is reachable stays un-ready (readiness pings the DB)
	// rather than crash-looping.
	dbURL := keys.Database.URL.Get(cfg)
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Error("open postgres", "error", err)
	}
	var store notifications.Store
	if db != nil {
		db.SetMaxOpenConns(4)
		db.SetMaxIdleConns(2)
		db.SetConnMaxLifetime(5 * time.Minute)
		lc.OnShutdown("postgres", func(_ context.Context) error { return db.Close() })
		hlth.AddReadinessCheck("postgres", func(ctx context.Context) error { return db.PingContext(ctx) })
		store = notifications.NewPostgresStore(db)
	}

	// NATS — the event source. Without it there's nothing to write, so readiness
	// fails when NATS is unavailable (same posture as the webhooks dispatcher).
	natsURL := keys.Notifications.NATSURL.Get(cfg)
	natsBus, err := natsbus.New(natsURL, constants.ServiceNotifications, log)
	if err != nil {
		log.Error("nats unavailable — no notifications will be written until it recovers", "error", err)
		hlth.AddReadinessCheck("nats", func(context.Context) error { return err })
	} else {
		lc.OnShutdown("nats", func(_ context.Context) error { return natsBus.Close() })
		ctx := context.Background()
		// Self-heal, don't latch: a pod that boots racing NATS/JetStream used
		// to fail every Subscribe once and then stay DEAF until manually
		// restarted (fresh stack 2026-07-26: all subjects failed "context
		// deadline exceeded" at boot; the notifications table stayed empty for
		// 5h). Retry each failed subject until it sticks — same doctrine as
		// webhooks + the warm caches.
		subscribeAll := func() []string {
			var pending []string
			if serr := natsBus.EnsureStream(ctx, events.StreamName, []string{events.StreamSubjects}); serr != nil {
				log.Warn("ensure stream", "error", serr)
			}
			for _, subject := range notificationSubjects {
				if serr := natsBus.Subscribe(ctx, subject, constants.NATSGroupNotifications, eventHandler(store, log)); serr != nil {
					log.Error("subscribe failed, will retry", "subject", subject, "error", serr)
					pending = append(pending, subject)
				}
			}
			return pending
		}
		if pending := subscribeAll(); len(pending) > 0 {
			go func() {
				for len(pending) > 0 {
					time.Sleep(15 * time.Second)
					var still []string
					for _, subject := range pending {
						if serr := natsBus.Subscribe(ctx, subject, constants.NATSGroupNotifications, eventHandler(store, log)); serr != nil {
							still = append(still, subject)
						} else {
							log.Info("subscribe established after retry", "subject", subject)
						}
					}
					pending = still
				}
			}()
		}
		log.Info("notifications consumer consuming events", "subjects", len(notificationSubjects))
	}

	metrics := middleware.NewMetrics(constants.ServiceNotifications)
	mux := http.NewServeMux()
	mux.Handle(routes.Healthz, hlth.LivenessHandler())
	mux.Handle(routes.Readyz, hlth.ReadinessHandler())
	mux.Handle(routes.Metrics, metrics.Handler())

	handler := tracing.HTTPMiddleware(constants.ServiceNotifications)(metrics.Wrap(middleware.CORS(mux)))
	server := &http.Server{Addr: ":" + port, Handler: handler, ReadTimeout: 5 * time.Second, WriteTimeout: 30 * time.Second}

	log.Info("notifications starting", "port", port)
	lifecycle.ServeHTTP(lc, server, log, 30*time.Second)
}

// eventHandler routes one NATS message: translate the payload → a notification
// row, insert it, ack. A payload with no account owner or an unmapped subject
// is a poison message (ack — no redelivery, nothing to show a user). A DB write
// failure is transient (nak — redeliver once the blip clears), matching the
// webhooks dispatcher's degrade-don't-crash posture.
func eventHandler(store notifications.Store, log *slog.Logger) events.Handler {
	return func(ctx context.Context, msg *events.Message) error {
		n, ok := notifications.Translate(msg.Subject, msg.Data)
		if !ok {
			log.Warn("dropping event with no account_id / unmapped subject", "subject", msg.Subject)
			return msg.Ack()
		}
		if store == nil {
			log.Error("notification store unavailable, will redeliver", "subject", msg.Subject, "account_id", n.AccountID)
			return msg.Nak()
		}
		if err := store.Insert(ctx, n); err != nil {
			log.Error("notification insert failed, will redeliver", "subject", msg.Subject, "account_id", n.AccountID, "error", err)
			return msg.Nak()
		}
		return msg.Ack()
	}
}
