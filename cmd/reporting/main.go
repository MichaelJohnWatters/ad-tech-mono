// cmd/reporting consumes events from NATS and writes to the analytics store.
// Unified with billing - every event write also accrues spend.
// Uses DuckDB locally and ClickHouse in production.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/billing"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache/warm"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events/natsbus"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/health"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
	_ "github.com/lib/pq"
)

func main() {
	log := logger.New(constants.ServiceReporting)
	sc := config.Setup(constants.ServiceReporting, log)
	config.PublishSchemaWithURL(sc.Cfg.Get("database.url", ""), constants.ServiceReporting, reportingSchema, log)
	cfg := sc.Cfg
	_ = sc
	hlth := health.New()
	lc := lifecycle.New(log)

	// OpenTelemetry — empty endpoint disables tracing so dev/test envs
	// without Jaeger still boot.
	otelShutdown := tracing.Init(context.Background(), tracing.Config{
		ServiceName:    constants.ServiceReporting,
		ServiceVersion: cfg.Get("otel.service_version", "dev"),
		Endpoint:       cfg.Get("otel.endpoint", "localhost:4318"),
		SampleRatio:    cfg.GetFloat("otel.sample_ratio", 1.0),
		Log:            log,
	})
	lc.OnShutdown("otel", func(ctx context.Context) error { return otelShutdown(ctx) })

	port := cfg.Get("reporting.port", routes.PortReporting)

	// Analytics store
	store := analytics.NewMemory()
	lc.OnShutdown("analytics-store", func(_ context.Context) error {
		return store.Close()
	})

	// Billing engine (unified with reporting - single consumer)
	clk := clock.Real{}
	ledger := billing.NewLedger()
	contracts := billing.NewContractStore()
	billingEngine := billing.NewEngine(ledger, contracts, clk, log)

	// Publisher contracts come from Postgres via a warm cache. Each refresh
	// re-populates the in-memory ContractStore the billing engine reads from,
	// so editing a publisher's revshare_config takes effect within one poll
	// (or instantly via the NATS invalidate subject).
	contractCache := startContractCache(cfg, clk, log, contracts)
	if contractCache != nil {
		lc.OnShutdown("contract-cache", func(_ context.Context) error { contractCache.Stop(); return nil })
	}

	// Event consumer with billing
	consumer := NewEventConsumer(log, store, billingEngine)

	// Connect to NATS for event consumption
	natsURL := cfg.Get("reporting.nats_url", routes.DefaultNATSURL)
	natsBus, err := natsbus.New(natsURL, constants.ServiceReporting, log)
	if err != nil {
		log.Warn("nats unavailable, events only via HTTP endpoint", "error", err)
	} else {
		lc.OnShutdown("nats", func(_ context.Context) error { return natsBus.Close() })
		// Ensure stream exists before subscribing
		ctx := context.Background()
		if err := natsBus.EnsureStream(ctx, "adtech", []string{"adtech.>"}); err != nil {
			log.Warn("failed to ensure stream", "error", err)
		}
		if err := consumer.RegisterNATSSubscriptions(natsBus); err != nil {
			log.Error("failed to register NATS subscriptions", "error", err)
		} else {
			log.Info("consuming events from NATS JetStream")
		}
	}

	metrics := middleware.NewMetrics(constants.ServiceReporting)

	mux := http.NewServeMux()
	mux.Handle(routes.Healthz, hlth.LivenessHandler())
	mux.Handle(routes.Readyz, hlth.ReadinessHandler())
	mux.Handle(routes.Metrics, metrics.Handler())

	// Query API
	mux.HandleFunc(routes.ReportingQuery, queryHandler(log, store))

	// HTTP event ingestion
	mux.HandleFunc(routes.ReportingEvents, consumer.HTTPHandler())

	if cfg.GetBool("debug.endpoints_enabled", true) && contractCache != nil {
		mux.HandleFunc(routes.DebugCacheRefresh, warm.RefreshHandler(contractCache))
	}

	// Debug: count auction-win records for a trace_id. Used by e2e tests to
	// assert auction.win NATS events reached the analytics store. Returns
	// {"count": N} so tests can verify exactly-once delivery (no dupes).
	// Gated by debug.endpoints_enabled to keep it off prod surface.
	if cfg.GetBool("debug.endpoints_enabled", true) {
		// store is *analytics.MemoryStore in dev — direct method call. When
		// DuckDB/ClickHouse get wired, replace this with a generic count
		// query via the Store.Query interface.
		mux.HandleFunc(routes.DebugAuctionWins, func(w http.ResponseWriter, r *http.Request) {
			traceID := r.URL.Query().Get("trace_id")
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			json.NewEncoder(w).Encode(map[string]int{"count": store.AuctionWinCount(traceID)})
		})
	}

	// Billing API
	mux.HandleFunc(routes.BillingSummary, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		json.NewEncoder(w).Encode(ledger.Summary())
	})
	mux.HandleFunc(routes.BillingLedger, func(w http.ResponseWriter, r *http.Request) {
		traceID := r.URL.Query().Get("trace_id")
		var entries []billing.LedgerEntry
		if traceID != "" {
			entries = ledger.EntriesForTrace(traceID)
		} else {
			entries = ledger.Entries()
		}
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		json.NewEncoder(w).Encode(entries)
	})

	handler := tracing.HTTPMiddleware(constants.ServiceReporting)(metrics.Wrap(middleware.CORS(mux)))
	server := &http.Server{Addr: ":" + port, Handler: handler, ReadTimeout: 5 * time.Second, WriteTimeout: 30 * time.Second}

	log.Info("reporting starting", "port", port)
	lifecycle.ServeHTTP(lc, server, log, 30*time.Second)
}

// EventConsumer processes events: writes to analytics store AND billing ledger.
type EventConsumer struct {
	log     *slog.Logger
	store   analytics.Store
	billing *billing.Engine
}

func NewEventConsumer(log *slog.Logger, store analytics.Store, billingEngine *billing.Engine) *EventConsumer {
	return &EventConsumer{log: log, store: store, billing: billingEngine}
}

// RegisterNATSSubscriptions sets up NATS consumers for all event subjects.
// Called when NATS is available.
func (c *EventConsumer) RegisterNATSSubscriptions(bus events.EventBus) error {
	subjects := map[string]events.Handler{
		events.SubjectImpression:     c.handleImpression,
		events.SubjectClick:          c.handleClick,
		events.SubjectConversion:     c.handleConversion,
		events.SubjectAuctionComplete: c.handleAuction,
		events.SubjectAuctionWin:     c.handleAuctionWin,
	}

	ctx := context.Background()
	for subject, handler := range subjects {
		if err := bus.Subscribe(ctx, subject, constants.NATSGroupReporting, handler); err != nil {
			return err
		}
		c.log.Info("subscribed to NATS subject", "subject", subject)
	}
	return nil
}

func (c *EventConsumer) handleImpression(ctx context.Context, msg *events.Message) error {
	var e analytics.ImpressionEvent
	if err := json.Unmarshal(msg.Data, &e); err != nil {
		c.log.Error("failed to decode impression event", "error", err)
		return msg.Ack() // don't redeliver bad data
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	if e.SchemaVersion == 0 {
		e.SchemaVersion = 1
	}

	if err := c.store.InsertImpression(ctx, &e); err != nil {
		c.log.Error("failed to write impression", "error", err, "trace_id", e.TraceID)
		return msg.Nak()
	}

	// Bill atomically with analytics write (unified consumer)
	if c.billing != nil {
		c.billing.ProcessEvent(ctx, billing.SpendEvent{
			TraceID: e.TraceID, CampaignID: e.CampaignID, CreativeID: e.CreativeID,
			PlacementID: e.PlacementID, PublisherID: e.PublisherID, AdvertiserID: e.AccountID,
			ClearingPrice: e.ClearingPriceUSD, Currency: "USD",
			BidModel: billing.BidModel(e.BidModel), DealType: e.DealID,
			EventType: "impression", Timestamp: e.Timestamp,
		})
	}

	c.log.Debug("impression recorded + billed", "trace_id", e.TraceID, "campaign_id", e.CampaignID)
	return msg.Ack()
}

func (c *EventConsumer) handleClick(ctx context.Context, msg *events.Message) error {
	var e analytics.ClickEvent
	if err := json.Unmarshal(msg.Data, &e); err != nil {
		c.log.Error("failed to decode click event", "error", err)
		return msg.Ack()
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	if e.SchemaVersion == 0 {
		e.SchemaVersion = 1
	}

	if err := c.store.InsertClick(ctx, &e); err != nil {
		c.log.Error("failed to write click", "error", err, "trace_id", e.TraceID)
		return msg.Nak()
	}

	c.log.Debug("click recorded", "trace_id", e.TraceID, "campaign_id", e.CampaignID)
	return msg.Ack()
}

func (c *EventConsumer) handleConversion(ctx context.Context, msg *events.Message) error {
	var e analytics.ConversionEvent
	if err := json.Unmarshal(msg.Data, &e); err != nil {
		c.log.Error("failed to decode conversion event", "error", err)
		return msg.Ack()
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	if e.SchemaVersion == 0 {
		e.SchemaVersion = 1
	}

	if err := c.store.InsertConversion(ctx, &e); err != nil {
		c.log.Error("failed to write conversion", "error", err, "trace_id", e.TraceID)
		return msg.Nak()
	}

	c.log.Debug("conversion recorded", "trace_id", e.TraceID, "campaign_id", e.CampaignID)
	return msg.Ack()
}

// handleAuctionWin lands the winning-bid event in the analytics store.
//
// Today this writes analytics only — billing accrual still happens on the
// impression pixel in handleImpression. The billing source-of-truth flip
// (auction.win → reservation, impression → settlement) is a separate
// migration; see docs/PLAN.md "Billing source-of-truth migration".
func (c *EventConsumer) handleAuctionWin(ctx context.Context, msg *events.Message) error {
	var src events.AuctionWinEvent
	if err := json.Unmarshal(msg.Data, &src); err != nil {
		c.log.Error("failed to decode auction win event", "error", err)
		return msg.Ack() // bad payload — don't redeliver
	}
	if src.Timestamp.IsZero() {
		src.Timestamp = time.Now()
	}

	e := analytics.AuctionWinEvent{
		TraceID:       src.TraceID,
		AuctionID:     src.AuctionID,
		WinnerDSP:     src.WinnerDSP,
		CampaignID:    src.CampaignID,
		CreativeID:    src.CreativeID,
		PlacementID:   src.PlacementID,
		PublisherID:   src.PublisherID,
		AdvertiserID:  src.AdvertiserID,
		ClearingPrice: src.ClearingPrice,
		Currency:      src.Currency,
		BidModel:      src.BidModel,
		DealID:        src.DealID,
		Channel:       src.Channel,
		SchemaVersion: 1,
		Timestamp:     src.Timestamp,
	}
	if err := c.store.InsertAuctionWin(ctx, &e); err != nil {
		c.log.Error("failed to write auction win", "error", err, "trace_id", e.TraceID)
		return msg.Nak()
	}
	c.log.Debug("auction win recorded", "trace_id", e.TraceID, "campaign_id", e.CampaignID, "price", e.ClearingPrice)
	return msg.Ack()
}

func (c *EventConsumer) handleAuction(ctx context.Context, msg *events.Message) error {
	var e analytics.AuctionEvent
	if err := json.Unmarshal(msg.Data, &e); err != nil {
		c.log.Error("failed to decode auction event", "error", err)
		return msg.Ack()
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	if e.SchemaVersion == 0 {
		e.SchemaVersion = 1
	}

	if err := c.store.InsertAuction(ctx, &e); err != nil {
		c.log.Error("failed to write auction", "error", err, "trace_id", e.TraceID)
		return msg.Nak()
	}

	c.log.Debug("auction recorded", "trace_id", e.TraceID, "placement_id", e.PlacementID)
	return msg.Ack()
}

// HTTPHandler provides an HTTP endpoint for ingesting events without NATS.
// Useful for testing and standalone mode.
func (c *EventConsumer) HTTPHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var batch []analytics.Event
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}

		if err := c.store.InsertBatch(r.Context(), batch); err != nil {
			c.log.Error("batch insert failed", "error", err)
			http.Error(w, "write failed", http.StatusInternalServerError)
			return
		}

		// Bill each impression in the batch
		if c.billing != nil {
			for _, ev := range batch {
				if ev.Type == analytics.EventImpression && ev.Impression != nil {
					imp := ev.Impression
					c.billing.ProcessEvent(r.Context(), billing.SpendEvent{
						TraceID: imp.TraceID, CampaignID: imp.CampaignID,
						PlacementID: imp.PlacementID, PublisherID: imp.PublisherID,
						AdvertiserID: imp.AccountID, ClearingPrice: imp.ClearingPriceUSD,
						Currency: "USD", BidModel: billing.BidModel(imp.BidModel),
						EventType: "impression", Timestamp: imp.Timestamp,
					})
				}
			}
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

// queryHandler serves analytics queries via HTTP.
func queryHandler(log *slog.Logger, store analytics.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var params analytics.QueryParams
		if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
			http.Error(w, "invalid query params", http.StatusBadRequest)
			return
		}

		result, err := store.Query(r.Context(), params)
		if err != nil {
			log.Error("query failed", "error", err)
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}

		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		json.NewEncoder(w).Encode(result)
	}
}

// startContractCache wires a warm cache that, on every successful refresh,
// re-populates the billing engine's in-memory ContractStore. Lookups in the
// billing hot path stay against the engine's existing store — the cache is
// the upstream source. Falls back to a no-op loader if Postgres is unreachable
// so the service still boots and uses the ContractStore's default fallback.
func startContractCache(cfg *config.Config, clk clock.Clock, log *slog.Logger, contracts *billing.ContractStore) *warm.Cache[postgres.ContractRow] {
	pollInterval := firstNonZeroDuration(
		cfg.GetDuration("cache.warm.billing_rates.poll_interval", 0),
		cfg.GetDuration("cache.warm.poll_interval", 300*time.Second),
	)
	loader := pickContractLoader(cfg, log)
	bus := connectInvalidateBus(cfg, log)

	c := warm.New(warm.Config[postgres.ContractRow]{
		Name:              "billing_rates",
		Loader:            loader,
		Clock:             clk,
		Bus:               bus,
		InvalidateSubject: events.SubjectCacheInvalidateBillingRates,
		PollInterval:      pollInterval,
		Log:               log,
		OnRefresh: func(_ context.Context, rows []postgres.ContractRow) {
			for _, r := range rows {
				contracts.Set(r.PublisherID, r.Contract)
			}
		},
	})
	if err := c.Start(context.Background()); err != nil {
		log.Warn("contract cache initial load failed (using fallback contracts)", "error", err)
	}
	return c
}

func pickContractLoader(cfg *config.Config, log *slog.Logger) warm.Loader[postgres.ContractRow] {
	dbURL := cfg.Get("database.url", "")
	if dbURL == "" {
		log.Warn("database.url not set, contract cache will be empty")
		return emptyContractLoader{}
	}
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Warn("postgres open failed", "error", err)
		return emptyContractLoader{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		log.Warn("postgres ping failed", "error", err)
		_ = db.Close()
		return emptyContractLoader{}
	}
	store, _ := postgres.New(postgres.Config{PrimaryURL: dbURL, MaxOpenConns: 3, MaxIdleConns: 1, ConnMaxLifetime: 5 * time.Minute})
	log.Info("postgres connected for contract loader")
	return &postgres.ContractLoader{Store: store}
}

type emptyContractLoader struct{}

func (emptyContractLoader) LoadAll(_ context.Context) ([]postgres.ContractRow, error) {
	return nil, nil
}
func (emptyContractLoader) KeyOf(r postgres.ContractRow) string { return r.PublisherID }

func connectInvalidateBus(cfg *config.Config, log *slog.Logger) events.EventBus {
	url := cfg.Get("reporting.nats_url", routes.DefaultNATSURL)
	bus, err := natsbus.New(url, constants.ServiceReporting+"-cache", log)
	if err != nil {
		log.Warn("nats unavailable for cache invalidate, polling only", "error", err)
		return nil
	}
	return bus
}

func firstNonZeroDuration(ds ...time.Duration) time.Duration {
	for _, d := range ds {
		if d > 0 {
			return d
		}
	}
	return 300 * time.Second
}
