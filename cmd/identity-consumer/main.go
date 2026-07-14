// cmd/identity-consumer builds the identity graph from observed identity
// signals. The SSP (and, later, other services) publish an ObservedEvent per
// request; this consumer batches, dedupes, and writes the resulting edges
// (deterministic co-occurrence + optional probabilistic IP+UA matching) to the
// identity_graph table.
//
// Splitting observation (cheap, on every serving pod) from the write (one
// consumer) keeps the write off the hot path and gives the probabilistic
// fingerprint state a single coherent view — run ONE replica for that reason.
package main

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"time"

	cacheredis "github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache/redis"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events/natsbus"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/health"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/identityobserve"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
	_ "github.com/lib/pq"
)

func main() {
	log := logger.New(constants.ServiceIdentityConsumer)
	sc := config.Setup(constants.ServiceIdentityConsumer, keys.IdentityConsumerSchema(), log)
	cfg := sc.Cfg
	hlth := health.New()
	lc := lifecycle.New(log)

	otelShutdown := tracing.Init(context.Background(), tracing.Config{
		ServiceName:    constants.ServiceIdentityConsumer,
		ServiceVersion: keys.Otel.ServiceVersion.Get(cfg),
		Endpoint:       keys.Otel.Endpoint.Get(cfg),
		SampleRatio:    keys.Otel.SampleRatio.Get(cfg),
		Log:            log,
	})
	lc.OnShutdown("otel", func(ctx context.Context) error { return otelShutdown(ctx) })

	port := keys.IdentityConsumer.Port.Get(cfg)

	// Postgres — the write target. Readiness pings it; without it we can't write.
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

	// Optional Redis-backed fingerprint buckets, so probabilistic matching stays
	// coherent if scaled beyond one replica. Falls back to in-memory (single
	// replica) when unset or unreachable.
	var fpStore identityobserve.FPStore
	if addr := keys.IdentityConsumer.RedisURL.Get(cfg); addr != "" {
		rctx, rcancel := context.WithTimeout(context.Background(), 3*time.Second)
		rc, rerr := cacheredis.New(rctx, cacheredis.Config{Addr: addr})
		rcancel()
		if rerr != nil {
			log.Warn("identity fingerprint redis unavailable, using in-memory buckets (single replica)", "error", rerr)
		} else {
			fpStore = newRedisFPStore(rc, keys.IdentityConsumer.FingerprintTTL.Get(cfg), log)
			lc.OnShutdown("fp-redis", func(_ context.Context) error { return rc.Close() })
			log.Info("identity fingerprint buckets: redis-backed")
		}
	}

	observer := identityobserve.New(postgres.NewFromDB(db), identityobserve.Config{
		Flush:       keys.IdentityConsumer.FlushInterval.Get(cfg),
		SeenCap:     keys.IdentityConsumer.SeenCap.Get(cfg),
		ProbEnabled: keys.IdentityConsumer.ProbabilisticEnabled.Get(cfg),
		ProbConf:    keys.IdentityConsumer.ProbabilisticConfidence.Get(cfg),
		FPMaxUsers:  keys.IdentityConsumer.FingerprintMaxUsers.Get(cfg),
		FuzzyUA:     keys.IdentityConsumer.FuzzyUA.Get(cfg),
		FPStore:     fpStore,
	}, log)
	observer.Start()
	lc.OnShutdown("identity-observer", func(_ context.Context) error { observer.Stop(); return nil })

	// NATS — the event source. Readiness fails without it.
	natsURL := keys.IdentityConsumer.NATSURL.Get(cfg)
	natsBus, err := natsbus.New(natsURL, constants.ServiceIdentityConsumer, log)
	if err != nil {
		log.Error("nats unavailable — no observations consumed until it recovers", "error", err)
		hlth.AddReadinessCheck("nats", func(context.Context) error { return err })
	} else {
		lc.OnShutdown("nats", func(_ context.Context) error { return natsBus.Close() })
		ctx := context.Background()
		if serr := natsBus.EnsureStream(ctx, events.StreamName, []string{events.StreamSubjects}); serr != nil {
			log.Warn("ensure stream", "error", serr)
		}
		if serr := natsBus.Subscribe(ctx, events.SubjectIdentityObserved, constants.NATSGroupIdentityConsumer, observeHandler(observer, log)); serr != nil {
			log.Error("subscribe failed", "subject", events.SubjectIdentityObserved, "error", serr)
		}
		log.Info("identity-consumer consuming observations")
	}

	metrics := middleware.NewMetrics(constants.ServiceIdentityConsumer)
	mux := http.NewServeMux()
	mux.Handle(routes.Healthz, hlth.LivenessHandler())
	mux.Handle(routes.Readyz, hlth.ReadinessHandler())
	mux.Handle(routes.Metrics, metrics.Handler())

	handler := tracing.HTTPMiddleware(constants.ServiceIdentityConsumer)(metrics.Wrap(middleware.CORS(mux)))
	server := &http.Server{Addr: ":" + port, Handler: handler, ReadTimeout: 5 * time.Second, WriteTimeout: 30 * time.Second}

	log.Info("identity-consumer starting", "port", port)
	lifecycle.ServeHTTP(lc, server, log, 30*time.Second)
}

// observeHandler decodes one observation and feeds it to the observer, then
// acks. A malformed payload is a poison message (ack — no redelivery); the
// observer itself is best-effort and never errors here, so we always ack.
func observeHandler(o *identityobserve.Observer, log *slog.Logger) events.Handler {
	return func(_ context.Context, msg *events.Message) error {
		ev, err := identityobserve.Unmarshal(msg.Data)
		if err != nil {
			log.Warn("dropping malformed identity observation", "error", err)
			return msg.Ack()
		}
		o.Observe(ev.IDs, ev.Fingerprint)
		return msg.Ack()
	}
}
