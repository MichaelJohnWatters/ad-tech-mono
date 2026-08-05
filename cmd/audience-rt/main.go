// cmd/audience-rt is the real-time retargeting consumer. It subscribes to the
// consent-gated behaviour stream (site visits) and the conversion stream, and
// enrolls visitors into — and suppresses converters from — the advertiser's
// retargeting segments the instant the event lands, instead of waiting for the
// hourly batch profile-builder. It writes the same audience_segment_members the
// batch builder would and invalidates the audience cache, so the DSP retargets on
// the fresh membership within seconds with no bid-time changes.
//
// Run ONE replica: it's a light stateless consumer; a single queue-group member
// keeps enroll/suppress ordering simple. Enrollment is idempotent regardless.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events/natsbus"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/health"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/retargeting"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
	_ "github.com/lib/pq"
)

// conversionPurchase is the conversion type that suppresses retargeting — a
// completed buy. Other conversion types (signup, lead) don't stop the chase.
const conversionPurchase = "purchase"

func main() {
	log := logger.New(constants.ServiceAudienceRT)
	sc := config.Setup(constants.ServiceAudienceRT, keys.AudienceRTSchema(), log)
	cfg := sc.Cfg
	hlth := health.New()
	lc := lifecycle.New(log)

	otelShutdown := tracing.Init(context.Background(), tracing.Config{
		ServiceName:    constants.ServiceAudienceRT,
		ServiceVersion: keys.Otel.ServiceVersion.Get(cfg),
		Endpoint:       keys.Otel.Endpoint.Get(cfg),
		SampleRatio:    keys.Otel.SampleRatio.Get(cfg),
		Log:            log,
	})
	lc.OnShutdown("otel", func(ctx context.Context) error { return otelShutdown(ctx) })

	port := keys.AudienceRT.Port.Get(cfg)

	// Postgres — the audience store (read segments, write/remove members).
	db, err := sql.Open("postgres", keys.Database.URL.Get(cfg))
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
	store := postgres.New(db)

	// NATS — the event source. Readiness fails without it.
	natsBus, err := natsbus.New(keys.AudienceRT.NATSURL.Get(cfg), constants.ServiceAudienceRT, log)
	if err != nil {
		log.Error("nats unavailable — no real-time retargeting until it recovers", "error", err)
		hlth.AddReadinessCheck("nats", func(context.Context) error { return err })
	} else {
		lc.OnShutdown("nats", func(_ context.Context) error { return natsBus.Close() })
		svc := retargeting.New(pgSource{store}, pgEnroller{store: store, bus: natsBus, log: log}, log)

		ctx := context.Background()
		natsBus.EnsureStreamWithRetry(ctx, events.StreamName, []string{events.StreamSubjects})
		// Self-heal, don't latch: a boot race that fails Subscribe once must not
		// leave the consumer DEAF forever — retry until it sticks (same doctrine
		// as identity-consumer / webhooks).
		subscribeAll := func() error {
			if e := natsBus.Subscribe(ctx, events.SubjectBehaviourObserved, constants.NATSGroupAudienceRT, behaviourHandler(svc, natsBus, log)); e != nil {
				return e
			}
			return natsBus.Subscribe(ctx, events.SubjectConversion, constants.NATSGroupAudienceRT, conversionHandler(svc, log))
		}
		if serr := subscribeAll(); serr != nil {
			log.Error("subscribe failed, will retry", "error", serr)
			go func() {
				for {
					time.Sleep(15 * time.Second)
					if subscribeAll() == nil {
						log.Info("subscribe established after retry")
						return
					}
				}
			}()
		}
		log.Info("audience-rt consuming behaviour + conversions")
	}

	// Storage hygiene: periodically delete retargeting members past their TTL.
	// Correctness doesn't depend on this (the read paths already exclude expired
	// rows) — it just stops dead rows accumulating. Runs on a ticker; a longer
	// interval is fine.
	purgeEvery := keys.AudienceRT.PurgeInterval.Get(cfg)
	purgeCtx, stopPurge := context.WithCancel(context.Background())
	lc.OnShutdown("purge-loop", func(_ context.Context) error { stopPurge(); return nil })
	go func() {
		t := time.NewTicker(purgeEvery)
		defer t.Stop()
		for {
			select {
			case <-purgeCtx.Done():
				return
			case <-t.C:
				n, err := store.PurgeExpiredMembers(purgeCtx)
				if err != nil {
					log.Warn("expired-member purge failed", "error", err)
				} else if n > 0 {
					log.Info("purged expired retargeting members", "rows", n)
				}
			}
		}
	}()

	metrics := middleware.NewMetrics(constants.ServiceAudienceRT)
	mux := http.NewServeMux()
	mux.Handle(routes.Healthz, hlth.LivenessHandler())
	mux.Handle(routes.Readyz, hlth.ReadinessHandler())
	mux.Handle(routes.Metrics, metrics.Handler())

	handler := tracing.HTTPMiddleware(constants.ServiceAudienceRT)(metrics.Wrap(middleware.CORS(mux)))
	server := &http.Server{Addr: ":" + port, Handler: handler, ReadTimeout: 5 * time.Second, WriteTimeout: 30 * time.Second}

	log.Info("audience-rt starting", "port", port)
	lifecycle.ServeHTTP(lc, server, log, 30*time.Second)
}

// behaviourHandler enrolls a site visitor into matching retargeting segments.
// Malformed payload = poison (ack, no redelivery); a transient store error naks
// so JetStream redelivers.
func behaviourHandler(svc *retargeting.Service, bus events.EventBus, log *slog.Logger) events.Handler {
	return func(ctx context.Context, msg *events.Message) error {
		var ev events.BehaviourSignalEvent
		if err := json.Unmarshal(msg.Data, &ev); err != nil {
			log.Warn("dropping malformed behaviour event", "error", err)
			return msg.Ack()
		}
		enrolled, err := svc.OnSiteVisit(ctx, ev)
		if err != nil {
			log.Warn("real-time enroll failed, will redeliver", "account", ev.AccountID, "trace_id", ev.TraceID, "error", err)
			return msg.Nak()
		}
		// Emit an account-scoped enrolled event per segment so the advertiser can
		// trigger an abandoned-cart push (via a webhook) the moment it happens.
		// ctx carries the site_visit's OTel trace context (NATS headers), so the
		// published event stays on the same trace; TraceID makes it explicit too.
		for _, segID := range enrolled {
			payload, _ := json.Marshal(events.RetargetingEnrolledEvent{
				SchemaVersion: events.CurrentSchemaVersion,
				TraceID:       ev.TraceID,
				AccountID:     ev.AccountID,
				SegmentID:     segID,
				UserID:        ev.UserID,
				Tag:           ev.Tag,
				EnrolledAt:    time.Now().UTC(),
			})
			if perr := bus.Publish(ctx, events.SubjectRetargetingEnrolled, payload); perr != nil {
				log.Warn("publish retargeting.enrolled failed", "account", ev.AccountID, "segment", segID, "trace_id", ev.TraceID, "error", perr)
			}
		}
		return msg.Ack()
	}
}

// conversionHandler suppresses a converter from the advertiser's retargeting
// segments on a completed purchase.
func conversionHandler(svc *retargeting.Service, log *slog.Logger) events.Handler {
	return func(ctx context.Context, msg *events.Message) error {
		var ev analytics.ConversionEvent
		if err := json.Unmarshal(msg.Data, &ev); err != nil {
			log.Warn("dropping malformed conversion event", "error", err)
			return msg.Ack()
		}
		if ev.ConversionType != conversionPurchase {
			return msg.Ack() // only a buy suppresses the chase
		}
		if _, err := svc.OnConversion(ctx, ev.AccountID, ev.UserID, ev.TraceID); err != nil {
			log.Warn("real-time suppress failed, will redeliver", "account", ev.AccountID, "trace_id", ev.TraceID, "error", err)
			return msg.Nak()
		}
		return msg.Ack()
	}
}

// pgSource adapts the audience Postgres store to retargeting.SegmentSource.
type pgSource struct{ store *postgres.Store }

func (s pgSource) RetargetingSegments(ctx context.Context, accountID string) ([]retargeting.Segment, error) {
	rows, err := s.store.RetargetingSegments(ctx, accountID)
	if err != nil {
		return nil, err
	}
	out := make([]retargeting.Segment, 0, len(rows))
	for _, r := range rows {
		vis := r.Visibility
		if vis == "" {
			vis = "dsp_private"
		}
		out = append(out, retargeting.Segment{ID: r.ID, AccountID: accountID, Type: "retargeting", Rule: r.Rule, Visibility: vis})
	}
	return out, nil
}

// pgEnroller adapts the store + NATS bus to retargeting.Enroller.
type pgEnroller struct {
	store *postgres.Store
	bus   events.EventBus
	log   *slog.Logger
}

// AddMembers/RemoveMember write memberships; the audience_segment_members trigger
// (migration 078) appends to the change-log in the same transaction, so there is
// no explicit changelog call here. The visibility arg is unused (the trigger reads
// it from the segment) but kept on the interface for callers that want it.
func (e pgEnroller) AddMembers(ctx context.Context, accountID, segmentID string, userIDs []string, ttl time.Duration, _, originTrace string) (int, error) {
	var expiresAt *time.Time
	if ttl > 0 {
		t := time.Now().Add(ttl)
		expiresAt = &t
	}
	// Lineage (migration 080): the enrolling site-visit's trace — first-enroll
	// only, a repeat visit refreshes expires_at without touching the origin.
	return e.store.AddMembersWithExpiry(ctx, accountID, segmentID, userIDs, expiresAt, "retargeting", originTrace)
}

func (e pgEnroller) RemoveMember(ctx context.Context, accountID, segmentID, userID, _ string) (int, error) {
	return e.store.RemoveMember(ctx, accountID, segmentID, userID)
}

// InvalidateAudience is now a no-op: the audience_segment_members trigger records
// enroll/suppress to the change-log and the single pipeline writer applies them to
// Redis within seconds, so there is nothing to publish. Kept to satisfy the
// retargeting.Enroller interface.
func (e pgEnroller) InvalidateAudience(_ context.Context, _ string) error { return nil }
