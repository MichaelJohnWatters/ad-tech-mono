// cmd/reporting consumes events from NATS and writes to the analytics store.
// Unified with billing - every event write also accrues spend.
// Uses DuckDB locally and ClickHouse in production.
package main

import (
	"context"
	"fmt"
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
	sc := config.Setup(constants.ServiceReporting, reportingSchema, log)
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
	ledger := selectLedger(cfg, log, lc)
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
		// Billing rates dump — read by the pub sim's Billing Rates
		// panel so operators can see which publisher contract a
		// settle resolved to. Returns the warm-cache snapshot
		// rather than re-querying Postgres so what's shown matches
		// what billing.Engine sees.
		mux.HandleFunc(routes.DebugBillingRates, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			rows := contractCache.All()
			out := make([]map[string]any, 0, len(rows))
			for _, c := range rows {
				row := map[string]any{
					"publisher_id": c.PublisherID,
				}
				if c.Contract != nil {
					row["currency"] = c.Contract.Currency
					row["model"] = string(c.Contract.Model)
					row["fee_pct"] = c.Contract.FeePct
					row["guaranteed_min_cpm"] = c.Contract.GuaranteedMinCPM
					row["tiers"] = c.Contract.Tiers
				}
				out = append(out, row)
			}
			_ = json.NewEncoder(w).Encode(out)
		})
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
			bidModel := r.URL.Query().Get("bid_model")
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			var count int
			if bidModel != "" {
				count = store.AuctionWinByBidModel(traceID, bidModel)
			} else {
				count = store.AuctionWinCount(traceID)
			}
			json.NewEncoder(w).Encode(map[string]int{"count": count})
		})

		mux.HandleFunc(routes.DebugBudgetDepletions, func(w http.ResponseWriter, r *http.Request) {
			campaignID := r.URL.Query().Get("campaign_id")
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			json.NewEncoder(w).Encode(map[string]int{"count": store.BudgetDepletionsByCampaign(campaignID)})
		})

		mux.HandleFunc(routes.DebugCampaignStateChanges, func(w http.ResponseWriter, r *http.Request) {
			campaignID := r.URL.Query().Get("campaign_id")
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			json.NewEncoder(w).Encode(store.CampaignStateChangesByCampaign(campaignID))
		})

		mux.HandleFunc(routes.DebugRenderFailures, func(w http.ResponseWriter, r *http.Request) {
			creativeID := r.URL.Query().Get("creative_id")
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			json.NewEncoder(w).Encode(store.RenderFailuresByCreative(creativeID))
		})

		mux.HandleFunc(routes.DebugFreqCapBlocks, func(w http.ResponseWriter, r *http.Request) {
			campaignID := r.URL.Query().Get("campaign_id")
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			json.NewEncoder(w).Encode(store.FreqCapBlocksByCampaign(campaignID))
		})

		mux.HandleFunc(routes.DebugTrackerRejections, func(w http.ResponseWriter, r *http.Request) {
			traceID := r.URL.Query().Get("trace_id")
			reason := r.URL.Query().Get("reason")
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			if traceID != "" {
				json.NewEncoder(w).Encode(store.TrackerRejectionsByTrace(traceID, reason))
				return
			}
			if reason != "" {
				json.NewEncoder(w).Encode(map[string]int{"count": store.TrackerRejectionsByReason(reason)})
				return
			}
			http.Error(w, `{"error":"trace_id or reason required"}`, http.StatusBadRequest)
		})

		mux.HandleFunc(routes.DebugServeNoFills, func(w http.ResponseWriter, r *http.Request) {
			traceID := r.URL.Query().Get("trace_id")
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			json.NewEncoder(w).Encode(map[string]int{"count": store.ServeNoFillsByTrace(traceID)})
		})

		mux.HandleFunc(routes.DebugMediaEvents, func(w http.ResponseWriter, r *http.Request) {
			traceID := r.URL.Query().Get("trace_id")
			channel := r.URL.Query().Get("channel")
			eventType := r.URL.Query().Get("event_type")
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			json.NewEncoder(w).Encode(map[string]int{"count": store.MediaEventsByTrace(traceID, channel, eventType)})
		})

		// Billing ledger reset — wipes in-memory ledger entries so e2e
		// billing tests can run in isolation. No-op on TigerBeetle backend
		// (the type assertion fails and we return reset=false).
		mux.HandleFunc(routes.DebugBillingReset, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			if mem, ok := ledger.(*billing.MemoryLedger); ok {
				mem.Reset()
				json.NewEncoder(w).Encode(map[string]any{"reset": true, "backend": "memory"})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"reset": false, "backend": "tigerbeetle"})
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
		events.SubjectImpression:         c.handleImpression,
		events.SubjectClick:              c.handleClick,
		events.SubjectConversion:         c.handleConversion,
		events.SubjectView:               c.handleView,
		events.SubjectAuctionComplete:    c.handleAuction,
		events.SubjectAuctionWin:         c.handleAuctionWin,
		events.SubjectDirectWin:          c.handleDirectWin,
		events.SubjectPrebidOutboundWin:  c.handlePrebidOutboundWin,
		events.SubjectBudgetDepleted:        c.handleBudgetDepleted,
		events.SubjectCampaignStateChanged:  c.handleCampaignState,
		events.SubjectTrackerRejected:       c.handleTrackerRejected,
		events.SubjectAdserverRenderFailed:    c.handleRenderFailed,
		events.SubjectAdserverFreqCapBlocked:  c.handleFreqCapBlocked,
		events.SubjectVideo:                 c.handleVideo,
		events.SubjectAudio:                 c.handleAudio,
		events.SubjectServeNoFill:           c.handleServeNoFill,
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

	// Settle the CPC reservation (if one exists). No-op for CPM (no
	// reservation), CPA (settles on conversion), vCPM (settles on view),
	// and CPCV (settles on complete). Original auction context is
	// recovered from the reservation row — click pixel doesn't carry
	// ClearingPrice/BidModel/DealType.
	if c.billing != nil {
		if _, err := c.billing.SettleByTrace(ctx, e.TraceID, "click"); err != nil {
			c.log.Warn("click settle failed", "trace_id", e.TraceID, "error", err)
		}
	}

	c.log.Debug("click recorded", "trace_id", e.TraceID, "campaign_id", e.CampaignID)
	return msg.Ack()
}

// handleView persists a viewability event. Mirrors handleImpression but
// without the billing call — vCPM settlement is a separate workstream (see
// tests/e2e/billing_models_test.go for placeholders).
func (c *EventConsumer) handleView(ctx context.Context, msg *events.Message) error {
	var e analytics.ViewEvent
	if err := json.Unmarshal(msg.Data, &e); err != nil {
		c.log.Error("failed to decode view event", "error", err)
		return msg.Ack()
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	if e.SchemaVersion == 0 {
		e.SchemaVersion = 1
	}

	if err := c.store.InsertView(ctx, &e); err != nil {
		c.log.Error("failed to write view", "error", err, "trace_id", e.TraceID)
		return msg.Nak()
	}

	// vCPM settlement. Picked defaults (see docs/PLAN.md → "vCPM Settlement Model"):
	//   1. Reserve at impression, settle at view — same reserve/settle
	//      shape used for CPC and CPA. Mechanism: SettleByTrace looks up
	//      the open reservation and routes through ProcessEvent.
	//   3. IABViewable=false → no settlement. Reservation stays open until
	//      the expiry cron releases it (pending, see TestBillingReservationExpiry).
	//      We deliberately do NOT downgrade to CPM rate here — "viewable
	//      or nothing" is the platform's default contract semantic.
	//   4. Rate source = the auction's clearing_price (already on the
	//      reservation row, recovered by SettleByTrace).
	//
	// Sub-decision 2 (the 24h reservation timeout) is honest but unbuilt;
	// reservations from unviewable / never-viewed impressions accumulate
	// in the in-memory ledger until restart. Pending the expiry cron.
	if c.billing != nil && e.IABViewable {
		if _, err := c.billing.SettleByTrace(ctx, e.TraceID, "viewable"); err != nil {
			c.log.Warn("view settle failed", "trace_id", e.TraceID, "error", err)
		}
	}

	c.log.Debug("view recorded",
		"trace_id", e.TraceID, "campaign_id", e.CampaignID,
		"iab_viewable", e.IABViewable, "duration_ms", e.DurationMs)
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

	// Settle the CPA reservation (if one exists). No-op for other models.
	if c.billing != nil {
		if _, err := c.billing.SettleByTrace(ctx, e.TraceID, "conversion"); err != nil {
			c.log.Warn("conversion settle failed", "trace_id", e.TraceID, "error", err)
		}
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

// handleDirectWin lands a publisher-adserver direct-sold serve in the
// analytics store. Reused AuctionWinEvent shape with BidModel="direct"
// so the existing analytics queries keep working — operators can filter
// `WHERE bid_model = 'direct'` to isolate direct serves. The publisher
// line item ID lives in CampaignID for symmetry with the programmatic
// path; demand_source string in AdvertiserID. WinnerDSP is the constant
// "publisher-adserver" so the source is always clear.
func (c *EventConsumer) handleDirectWin(ctx context.Context, msg *events.Message) error {
	var src events.DirectWinEvent
	if err := json.Unmarshal(msg.Data, &src); err != nil {
		c.log.Error("failed to decode direct win event", "error", err)
		return msg.Ack()
	}
	if src.Timestamp.IsZero() {
		src.Timestamp = time.Now()
	}
	e := analytics.AuctionWinEvent{
		TraceID:       src.TraceID,
		AuctionID:     src.TraceID, // no separate auction id for direct serves
		WinnerDSP:     "publisher-adserver",
		CampaignID:    src.PublisherLineItemID,
		CreativeID:    src.CreativeID,
		PlacementID:   src.PlacementID,
		PublisherID:   src.PublisherID,
		AdvertiserID:  src.DemandSource,
		ClearingPrice: src.CPM,
		Currency:      src.Currency,
		BidModel:      "direct:" + src.PriorityTier, // direct:sponsorship / direct:guaranteed / direct:house
		Channel:       "display",
		SchemaVersion: 1,
		Timestamp:     src.Timestamp,
	}
	if err := c.store.InsertAuctionWin(ctx, &e); err != nil {
		c.log.Error("failed to write direct win", "error", err, "trace_id", e.TraceID)
		return msg.Nak()
	}
	c.log.Debug("direct win recorded", "trace_id", e.TraceID, "line_item", src.PublisherLineItemID, "tier", src.PriorityTier, "cpm", src.CPM)
	return msg.Ack()
}

// handlePrebidOutboundWin lands an external-Prebid-wins-against-SSP serve
// in the analytics store. BidModel="prebid_outbound" so operators can
// filter for these specifically. WinnerDSP carries the external Prebid
// endpoint URL — most natural place since that endpoint IS the "demand
// source" for this path. We don't bill these (money moves outside our
// system); analytics is the only consumer.
func (c *EventConsumer) handlePrebidOutboundWin(ctx context.Context, msg *events.Message) error {
	var src events.PrebidOutboundWinEvent
	if err := json.Unmarshal(msg.Data, &src); err != nil {
		c.log.Error("failed to decode prebid outbound win event", "error", err)
		return msg.Ack()
	}
	if src.Timestamp.IsZero() {
		src.Timestamp = time.Now()
	}
	e := analytics.AuctionWinEvent{
		TraceID:       src.TraceID,
		AuctionID:     src.TraceID,
		WinnerDSP:     src.PrebidEndpoint,
		PlacementID:   src.PlacementID,
		PublisherID:   src.PublisherID,
		AdvertiserID:  src.Seat,
		ClearingPrice: src.ClearingPrice,
		Currency:      src.Currency,
		BidModel:      "prebid_outbound",
		DealID:        src.DealID,
		Channel:       "display",
		SchemaVersion: 1,
		Timestamp:     src.Timestamp,
	}
	if err := c.store.InsertAuctionWin(ctx, &e); err != nil {
		c.log.Error("failed to write prebid outbound win", "error", err, "trace_id", e.TraceID)
		return msg.Nak()
	}
	c.log.Debug("prebid outbound win recorded", "trace_id", e.TraceID, "endpoint", src.PrebidEndpoint, "price", src.ClearingPrice)
	return msg.Ack()
}

// handleVideo / handleAudio land video/audio engagement pings in the
// shared MediaEvent bucket. One handler per subject so the consumer-name
// suffix is distinct (events-video vs events-audio), but the storage
// shape is shared (Channel column distinguishes).
func (c *EventConsumer) handleVideo(ctx context.Context, msg *events.Message) error {
	var src events.VideoEvent
	if err := json.Unmarshal(msg.Data, &src); err != nil {
		c.log.Error("failed to decode video event", "error", err)
		return msg.Ack()
	}
	if src.Timestamp.IsZero() {
		src.Timestamp = time.Now()
	}
	if err := c.store.InsertMediaEvent(ctx, &analytics.MediaEvent{
		TraceID: src.TraceID, Channel: "video",
		EventType: src.EventType, PositionMs: src.PositionMs,
		Timestamp: src.Timestamp,
	}); err != nil {
		c.log.Error("insert video media event", "error", err, "trace_id", src.TraceID)
	}
	return msg.Ack()
}
func (c *EventConsumer) handleAudio(ctx context.Context, msg *events.Message) error {
	var src events.AudioEvent
	if err := json.Unmarshal(msg.Data, &src); err != nil {
		c.log.Error("failed to decode audio event", "error", err)
		return msg.Ack()
	}
	if src.Timestamp.IsZero() {
		src.Timestamp = time.Now()
	}
	if err := c.store.InsertMediaEvent(ctx, &analytics.MediaEvent{
		TraceID: src.TraceID, Channel: "audio",
		EventType: src.EventType, PositionMs: src.PositionMs,
		Timestamp: src.Timestamp,
	}); err != nil {
		c.log.Error("insert audio media event", "error", err, "trace_id", src.TraceID)
	}
	return msg.Ack()
}

// handleServeNoFill records the moment a publisher-adserver request fell
// through every demand source. Required for fill-rate analytics — the
// AuctionWin records served impressions; this records the misses.
func (c *EventConsumer) handleServeNoFill(ctx context.Context, msg *events.Message) error {
	var src events.ServeNoFillEvent
	if err := json.Unmarshal(msg.Data, &src); err != nil {
		c.log.Error("failed to decode serve nofill event", "error", err)
		return msg.Ack()
	}
	if src.Timestamp.IsZero() {
		src.Timestamp = time.Now()
	}
	if mem, ok := c.store.(*analytics.MemoryStore); ok {
		mem.InsertServeNoFill(analytics.ServeNoFill{
			TraceID:     src.TraceID,
			PublisherID: src.PublisherID,
			PlacementID: src.PlacementID,
			Reason:      src.Reason,
			Timestamp:   src.Timestamp,
		})
	}
	c.log.Info("serve nofill recorded", "trace_id", src.TraceID, "publisher", src.PublisherID, "reason", src.Reason)
	return msg.Ack()
}

// handleFreqCapBlocked records a serve suppression (user/campaign
// freq-cap counter saturated). No billing impact — no impression
// fired, no spend. Useful for ops dashboards (alert on suppression
// rate change) and advertiser reports ("we suppressed N over-cap
// impressions for your campaign").
func (c *EventConsumer) handleFreqCapBlocked(ctx context.Context, msg *events.Message) error {
	var src events.AdserverFreqCapBlockedEvent
	if err := json.Unmarshal(msg.Data, &src); err != nil {
		c.log.Error("failed to decode freq_cap_blocked event", "error", err)
		return msg.Ack()
	}
	if src.Timestamp.IsZero() {
		src.Timestamp = time.Now()
	}
	if mem, ok := c.store.(*analytics.MemoryStore); ok {
		mem.InsertFreqCapBlock(analytics.FreqCapBlock{
			TraceID:     src.TraceID,
			UserID:      src.UserID,
			CampaignID:  src.CampaignID,
			PlacementID: src.PlacementID,
			PublisherID: src.PublisherID,
			Timestamp:   src.Timestamp,
		})
	}
	c.log.Info("adserver freq cap block recorded",
		"trace_id", src.TraceID,
		"user_id", src.UserID,
		"campaign_id", src.CampaignID)
	return msg.Ack()
}

// handleRenderFailed records an ad-server fallback (creative miss or
// render error). No billing impact — the fallback HTML still produced
// an impression-pixel-eligible response. Ops dashboards alert on
// rate-of-change; advertisers see "creative X is broken in adserver"
// without scraping logs.
func (c *EventConsumer) handleRenderFailed(ctx context.Context, msg *events.Message) error {
	var src events.AdserverRenderFailedEvent
	if err := json.Unmarshal(msg.Data, &src); err != nil {
		c.log.Error("failed to decode render failed event", "error", err)
		return msg.Ack()
	}
	if src.Timestamp.IsZero() {
		src.Timestamp = time.Now()
	}
	if mem, ok := c.store.(*analytics.MemoryStore); ok {
		mem.InsertRenderFailure(analytics.RenderFailure{
			TraceID:     src.TraceID,
			CampaignID:  src.CampaignID,
			CreativeID:  src.CreativeID,
			PlacementID: src.PlacementID,
			PublisherID: src.PublisherID,
			Reason:      src.Reason,
			Detail:      src.Detail,
			Timestamp:   src.Timestamp,
		})
	}
	c.log.Info("adserver render failure recorded",
		"trace_id", src.TraceID,
		"creative_id", src.CreativeID,
		"reason", src.Reason,
		"detail", src.Detail)
	return msg.Ack()
}

// handleTrackerRejected records a tracker-published rejection (invalid
// HMAC in strict mode, fraud check blocked, dedup hit). No billing
// impact — the rejection by definition prevented any billing event
// from firing. Useful purely for ops dashboards (fraud-volume alert)
// and advertiser reports ("we blocked X% of fraudulent traffic").
func (c *EventConsumer) handleTrackerRejected(ctx context.Context, msg *events.Message) error {
	var src events.TrackerRejectedEvent
	if err := json.Unmarshal(msg.Data, &src); err != nil {
		c.log.Error("failed to decode tracker rejected event", "error", err)
		return msg.Ack()
	}
	if src.Timestamp.IsZero() {
		src.Timestamp = time.Now()
	}
	if mem, ok := c.store.(*analytics.MemoryStore); ok {
		mem.InsertTrackerRejection(analytics.TrackerRejection{
			TraceID:   src.TraceID,
			EventType: src.EventType,
			Reason:    src.Reason,
			Detail:    src.Detail,
			Timestamp: src.Timestamp,
		})
	}
	c.log.Info("tracker rejection recorded",
		"trace_id", src.TraceID,
		"event_type", src.EventType,
		"reason", src.Reason,
		"detail", src.Detail)
	return msg.Ack()
}

// handleCampaignState records a DSP-published state transition
// (live → paused, paused → live, → archived, → ended). Lets
// reporting dashboards show pause/resume timelines without waiting
// for the next 30s warm-cache poll, and e2e tests can assert the
// event propagated. No billing impact — pacing already stops bids on
// state != "live" via the warm cache filter.
func (c *EventConsumer) handleCampaignState(ctx context.Context, msg *events.Message) error {
	var src events.CampaignStateEvent
	if err := json.Unmarshal(msg.Data, &src); err != nil {
		c.log.Error("failed to decode campaign state event", "error", err)
		return msg.Ack()
	}
	if src.Timestamp.IsZero() {
		src.Timestamp = time.Now()
	}
	if mem, ok := c.store.(*analytics.MemoryStore); ok {
		mem.InsertCampaignStateChange(analytics.CampaignStateChange{
			CampaignID: src.CampaignID,
			AccountID:  src.AccountID,
			OldState:   src.OldState,
			NewState:   src.NewState,
			Reason:     src.Reason,
			Timestamp:  src.Timestamp,
		})
	}
	c.log.Info("campaign state changed",
		"campaign_id", src.CampaignID,
		"old", src.OldState,
		"new", src.NewState,
		"reason", src.Reason)
	return msg.Ack()
}

// handleBudgetDepleted records a DSP's "this campaign just ran out of
// budget" event. Lets reporting dashboards show "X campaigns went dark
// today" without scraping logs. No billing impact — depletion just stops
// further bids; the spend that got us there was already recorded via
// AuctionWin.
func (c *EventConsumer) handleBudgetDepleted(ctx context.Context, msg *events.Message) error {
	var src events.BudgetDepletedEvent
	if err := json.Unmarshal(msg.Data, &src); err != nil {
		c.log.Error("failed to decode budget depleted event", "error", err)
		return msg.Ack()
	}
	if src.Timestamp.IsZero() {
		src.Timestamp = time.Now()
	}
	if mem, ok := c.store.(*analytics.MemoryStore); ok {
		mem.InsertBudgetDepletion(analytics.BudgetDepletion{
			CampaignID: src.CampaignID,
			AccountID:  src.AccountID,
			Budget:     src.Budget,
			Spent:      src.Spent,
			Timestamp:  src.Timestamp,
		})
	}
	c.log.Info("campaign budget depleted", "campaign_id", src.CampaignID, "budget", src.Budget, "spent", src.Spent)
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

// pickContractLoader returns a self-healing warm.Loader. Lazy-opens
// Postgres on first LoadAll and reconnects after any error, so a
// reporting pod that boots before Postgres is reachable picks up
// contracts automatically on the next 30s poll. Mirrors the pattern
// from pkg/secrets and pickDealLoader in cmd/exchange.
func pickContractLoader(cfg *config.Config, log *slog.Logger) warm.Loader[postgres.ContractRow] {
	dbURL := cfg.Get("database.url", "")
	return &warm.RetryingLoader[postgres.ContractRow]{
		Log:   log,
		KeyFn: func(r postgres.ContractRow) string { return r.PublisherID },
		Construct: func() (warm.Loader[postgres.ContractRow], error) {
			if dbURL == "" {
				return nil, fmt.Errorf("database.url not set")
			}
			store, err := postgres.New(postgres.Config{PrimaryURL: dbURL, MaxOpenConns: 3, MaxIdleConns: 1, ConnMaxLifetime: 5 * time.Minute})
			if err != nil {
				return nil, fmt.Errorf("postgres connect: %w", err)
			}
			return &postgres.ContractLoader{Store: store}, nil
		},
	}
}

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
