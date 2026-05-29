// cmd/reporting consumes events from NATS and writes to the analytics store.
// Unified with billing - every event write also accrues spend.
// Uses DuckDB locally and ClickHouse in production.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/billing"
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
)

func main() {
	log := logger.New(constants.ServiceReporting)
	sc := config.Setup(constants.ServiceReporting, log)
	cfg := sc.Cfg
	_ = sc
	hlth := health.New()
	lc := lifecycle.New(log)

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

	// Seed demo publisher contracts
	contracts.Set("pub-daily-news", &billing.Contract{
		Model: billing.ModelTiered, FeePct: 20,
		Tiers: []billing.Tier{
			{MinImpressions: 0, MaxImpressions: 1000000, FeePct: 25},
			{MinImpressions: 1000000, MaxImpressions: 10000000, FeePct: 20},
			{MinImpressions: 10000000, MaxImpressions: 0, FeePct: 15},
		},
		DealTypeModifiers: map[string]float64{"pmp": -5, "pg": -10},
		Currency: "USD",
	})
	contracts.Set("sim-publisher", &billing.Contract{
		Model: billing.ModelFixed, FeePct: 20, Currency: "USD",
	})
	contracts.Set("trace-explorer-pub", &billing.Contract{
		Model: billing.ModelFixed, FeePct: 18, Currency: "USD",
	})

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

	mux := http.NewServeMux()
	mux.Handle(routes.Healthz, hlth.LivenessHandler())
	mux.Handle(routes.Readyz, hlth.ReadinessHandler())

	// Query API
	mux.HandleFunc(routes.ReportingQuery, queryHandler(log, store))

	// HTTP event ingestion
	mux.HandleFunc(routes.ReportingEvents, consumer.HTTPHandler())

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

	handler := middleware.CORS(mux)
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
	}

	ctx := context.Background()
	for subject, handler := range subjects {
		if err := bus.Subscribe(ctx, subject, "reporting", handler); err != nil {
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
