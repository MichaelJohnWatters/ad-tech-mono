// cmd/reporting consumes events from NATS and writes to the analytics store.
// Unified with billing - every event write also accrues spend.
// Uses DuckDB locally and ClickHouse in production.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/billing"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache/warm"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events/natsbus"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/health"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/reporting"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
	_ "github.com/lib/pq"
)

func main() {
	log := logger.New(constants.ServiceReporting)
	sc := config.Setup(constants.ServiceReporting, keys.ReportingSchema(), log)
	cfg := sc.Cfg
	_ = sc
	hlth := health.New()
	lc := lifecycle.New(log)

	// OpenTelemetry — empty endpoint disables tracing so dev/test envs
	// without Jaeger still boot.
	otelShutdown := tracing.Init(context.Background(), tracing.Config{
		ServiceName:    constants.ServiceReporting,
		ServiceVersion: keys.Otel.ServiceVersion.Get(cfg),
		Endpoint:       keys.Otel.Endpoint.Get(cfg),
		SampleRatio:    keys.Otel.SampleRatio.Get(cfg),
		Log:            log,
	})
	lc.OnShutdown("otel", func(ctx context.Context) error { return otelShutdown(ctx) })

	port := keys.Reporting.Port.Get(cfg)

	// Analytics store — memory (volatile, default) or durable DuckDB,
	// selected by reporting.analytics_backend. Core events flow through
	// the analytics.Store interface so they persist on whichever backend.
	store := selectAnalyticsStore(cfg, log)
	// Route deep-history reads to the Parquet lake when the cold store is enabled and
	// the binary was built with the duckdb tag; otherwise this is a no-op and
	// the hot store answers everything.
	store = maybeWrapHotCold(store, cfg, log)
	lc.OnShutdown("analytics-store", func(_ context.Context) error {
		return store.Close()
	})
	// dbg is the backend-agnostic read-back view for the /debug endpoints.
	// Both MemoryStore and ClickHouse implement analytics.DebugReader, so the
	// endpoints work on the full-local (clickhouse) stack as well as memory.
	// DuckDB doesn't implement it yet → those endpoints degrade to 501 there.
	// When the cold store wraps the hot backend, unwrap — debug reads are
	// recent-window by definition, so the hot store answers them.
	dbg, _ := store.(analytics.DebugReader)
	if dbg == nil {
		if hc, ok := store.(*analytics.HotColdStore); ok {
			dbg, _ = hc.HotStore().(analytics.DebugReader)
		}
	}

	// Billing engine (unified with reporting - single consumer)
	clk := clock.Real{}
	ledger := selectLedger(cfg, log, lc)
	// The TB client has a wedge failure mode (post clock-regression) where
	// writes hang/error-storm while the pod stays Ready and billing deltas
	// silently read zero. Surface it on /readyz (house rule: dep failures →
	// 503) so the wedge is visible the moment it happens instead of at the
	// next billing reconciliation. Recovery is still a pod restart. Memory
	// backend has no Health method and registers nothing.
	if hl, ok := ledger.(interface{ Health(context.Context) error }); ok {
		hlth.AddReadinessCheck("billing-ledger", hl.Health)
	}
	contracts := billing.NewContractStore()
	billingEngine := billing.NewEngine(ledger, contracts, clk, log)
	// Multi-currency: normalize non-USD spend events against exchange_rates
	// before any money moves (unknown currency = loud unbillable drop, never
	// a silent 1:1 booking). Rates cached per (currency, day).
	billingEngine.SetRateSource(newPGRateSource(cfg.Get(keys.Database.URL.Key(), ""), log).Rate)

	// Prepay drawdown (money loop): every realized spend the engine bills or
	// settles also debits advertiser_balances and pings the DSP's balance
	// cache. Nil bus is tolerated (the DSP's poll keeps it fresh).
	startBalanceSink(cfg, log, billingEngine, connectInvalidateBus(cfg, log))

	// Reserve/settle context store: reserve persists the auction context so
	// settle can recover publisher/advertiser/campaign on a ledger backend
	// (TigerBeetle) that can't retain strings. Without this, CPC/CPA/vCPM
	// settle + drawdown break under TB.
	startReservationStore(cfg, log, billingEngine)

	// Restart-safety for DSP pacing: hydrate today's committed spend from
	// Postgres BEFORE event consumption starts, so a reporting restart resumes
	// the day's total instead of resetting it (which would reconcile every DSP
	// budget counter down and risk overspend). The same store persists each
	// snapshot tick (wired into the publisher below).
	committedSpendStore := newCommittedSpendStore(cfg, log)
	hydrateCommittedSpend(committedSpendStore, billingEngine, clk, log)

	// Multi-replica pacing: when reporting.shared_pacing_counter is on, point the
	// engine's committed snapshot at a shared Redis counter (additive across
	// replicas) + a periodic store-backed reconcile, so >1 reporting replica can
	// run without fragmenting DSP pacing. Opt-in; off = in-memory accumulator,
	// single-replica behaviour unchanged. Runs before the snapshot publisher so
	// its boot seed lands first. See committed_counter.go.
	authoritativeSettled := startSharedPacingCounter(billingEngine, store, cfg, clk, log, lc)

	// Publisher contracts come from Postgres via a warm cache. Each refresh
	// re-populates the in-memory ContractStore the billing engine reads from,
	// so editing a publisher's revshare_config takes effect within one poll
	// (or instantly via the NATS invalidate subject).
	// The analytics store feeds tiered revenue-share the publisher's
	// month-to-date impression count (nil if the backend can't answer).
	impReader, _ := store.(analytics.PublisherImpressionReader)
	contractCache := startContractCache(cfg, clk, log, contracts, impReader)
	if contractCache != nil {
		lc.OnShutdown("contract-cache", func(_ context.Context) error { contractCache.Stop(); return nil })
	}

	// Rollup engine — aggregates raw events into minute/hour/day/month
	// rows and persists them via the analytics store's RollupWriter. The
	// scheduler only runs when reporting.rollup_enabled (off by default);
	// the engine is always built so /debug/rollup/run works regardless.
	rollupEngine := newRollupEngine(store, clk, log)
	startRollupScheduler(rollupEngine, cfg, clk, log, lc)

	// Event consumer with billing
	consumer := NewEventConsumer(log, store, billingEngine)
	// Deal-type fee modifiers: resolve the beacon's deal id to its TYPE so
	// Contract.DealTypeModifiers can match (the raw id never did).
	consumer.SetDealTypeResolver(newPGDealTypeSource(cfg.Get(keys.Database.URL.Key(), ""), log).For)

	// Data-monetization accrual (ADR 0009): parked DataFeeEvents settle when
	// the impression for their trace arrives. Needs Postgres (the durable
	// pending join + earnings + ledger); absent DB → feature off with a WARN,
	// everything else unaffected.
	if dfURL := cfg.Get(keys.Database.URL.Key(), ""); dfURL != "" {
		if dfDB, err := sql.Open("postgres", dfURL); err == nil {
			dfDB.SetMaxOpenConns(3)
			dfDB.SetMaxIdleConns(1)
			lc.OnShutdown("datafee-db", func(_ context.Context) error { return dfDB.Close() })
			consumer.SetDataFeeAccrual(newDataFeeAccrual(dfDB,
				func() float64 { return keys.Reporting.DataFeeMarginPct.Get(cfg) }, log))
			// Data-marketplace surcharge settlement (PLAN Phase 10) shares the DB.
			consumer.SetMarketplaceAccrual(newMarketplaceAccrual(dfDB,
				func() float64 { return keys.Reporting.MarketplaceSurchargeMarginPct.Get(cfg) }, log))
		} else {
			log.Warn("data-fee accrual disabled (postgres open failed)", "error", err)
		}
	} else {
		log.Warn("data-fee accrual disabled (database.url not set)")
	}

	// View-through conversion attribution (Phase 2). Credits a click-less
	// conversion to a prior viewable exposure. Needs the store's view-through
	// lookback; cross-device resolution additionally needs the Postgres identity
	// graph (absent → same-id matching only). Both are nil-tolerant.
	if vr, ok := store.(analytics.ViewThroughReader); ok {
		var idResolver identityResolver
		if dbURL := cfg.Get(keys.Database.URL.Key(), ""); dbURL != "" {
			// Lazy + self-healing: opens on first resolve, retries on later ones.
			// Never latch cross-device attribution off on a boot-time DB blip
			// (a fresh install starts this pod before Postgres resolves).
			idResolver = &lazyIdentityResolver{url: dbURL, lc: lc, log: log}
		}
		aw, _ := store.(analytics.AttributionWriter) // nil → no multi-touch chain capture
		var attrOverrides *pgAttributionConfigSource
		if dbURL := cfg.Get(keys.Database.URL.Key(), ""); dbURL != "" {
			attrOverrides = newPGAttributionConfigSource(dbURL, log) // per-line-item windows; fail-open to globals
		}
		consumer.SetViewThroughAttributor(newViewThroughAttributor(vr, aw, idResolver, attrOverrides, cfg, log))
		log.Info("view-through attribution enabled", "cross_device", idResolver != nil, "multitouch", aw != nil, "per_campaign_overrides", attrOverrides != nil)
	} else {
		log.Warn("view-through attribution disabled (analytics store lacks view-through lookback)")
	}

	// Bulk NATS consumer for high-volume core events. Default ON: single-row
	// ClickHouse inserts can't keep up with sustained traffic (~2/sec) and pile
	// up MergeTree parts (the "too many broken parts" failure). Requires a
	// backend that supports bulk inserts (ClickHouse today); on memory/duckdb
	// the flag is a no-op warning and the per-message path is used. Dedup
	// (Redis SetNX on the stream sequence) makes redelivery idempotent. Set
	// reporting.clickhouse_batch_consumer=false to force the per-message path.
	if keys.Reporting.ClickHouseBatchConsumer.Get(cfg) {
		if bi, ok := store.(analytics.BatchInserter); ok {
			dedup := cache.NewDedupAdapter(connectReportingRedis(cfg, log))
			ttl := keys.Reporting.DedupTTL.Get(cfg)
			consumer.EnableBatchConsumer(bi, dedup, ttl)
			log.Info("reporting: batch consumer enabled for core events", "dedup_ttl", ttl)
		} else {
			log.Warn("reporting.clickhouse_batch_consumer set but analytics backend has no bulk-insert support; using per-message path")
		}
	}

	// Connect to NATS for event consumption
	natsURL := keys.Reporting.NATSURL.Get(cfg)
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
		// Broadcast per-campaign committed spend so DSPs reconcile pacing to
		// billed reality. Needs NATS, so it lives inside this branch.
		startSpendSnapshotPublisher(billingEngine, natsBus, committedSpendStore, authoritativeSettled, cfg, clk, log, lc)
	}

	metrics := middleware.NewMetrics(constants.ServiceReporting)
	// Overspend watchdog: negative advertiser balances = the DSP balance
	// gate admitted more spend than funding. Expected 0 — this is the
	// continuous empirical check on the gate's staleness budget. Small
	// bounded overspend is the DESIGN (prepay dips slightly negative, gate
	// closes, next topup recovers the debt); this gauge keeps it visible.
	startOverspendGauge(context.Background(), cfg.Get(keys.Database.URL.Key(), ""), metrics.Registry(), log)

	mux := http.NewServeMux()
	// On-demand profiler (internal mux only; zero cost until a profile is
	// pulled). Block/mutex profiling stays off until armed — see
	// middleware.SetProfileRates.
	middleware.AttachPprof(mux)
	// First-class rollup trigger (the batch-conductor chain step drives
	// this) — registered unconditionally, unlike the /debug alias below.
	mux.HandleFunc(routes.ReportingRollupRun, rollupRunHandler(rollupEngine, log))
	// ADR 0006 phase 4: hourly ClickHouse→Parquet export (idempotent per hour).
	mux.HandleFunc(routes.ReportingAttribution, attributionHandler(store, log))
	mux.HandleFunc(routes.ReportingExportRun, exportRunHandler(store, cfg, log))
	// ADR 0006 phase 5: export reconciliation (replaces the retired Delta snapshot).
	mux.HandleFunc(routes.ReportingExportSnapshot, exportSnapshotHandler(store, cfg, log))
	mux.Handle(routes.Healthz, hlth.LivenessHandler())
	mux.Handle(routes.Readyz, hlth.ReadinessHandler())
	mux.Handle(routes.Metrics, metrics.Handler())

	// Query API
	// The metrics engine computes derived metrics (ecpm/ctr/fill_rate/
	// net_revenue) server-side. It's a drop-in for the raw store: queries
	// without derived metrics pass straight through unchanged.
	engine := reporting.NewQueryEngine(store, contractNet{contracts})
	mux.HandleFunc(routes.ReportingQuery, queryHandler(log, engine, func() time.Duration {
		return keys.Reporting.QueryTimeout.Get(cfg)
	}))
	mux.HandleFunc(routes.ReportingChannels, channelBreakdownHandler(engine, log))

	// HTTP event ingestion
	mux.HandleFunc(routes.ReportingEvents, consumer.HTTPHandler())

	if keys.Debug.EndpointsEnabled.Get(cfg) && contractCache != nil {
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
	if keys.Debug.EndpointsEnabled.Get(cfg) {
		// These read-back endpoints are MemoryStore-only (used by the e2e
		// harness, which runs the memory backend). memGuard returns false
		// and writes 501 when reporting.analytics_backend isn't memory, so
		// the duckdb backend degrades honestly instead of nil-panicking.
		// Core event persistence is unaffected — only these dev affordances.
		memGuard := func(w http.ResponseWriter) bool {
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			if dbg == nil {
				http.Error(w, `{"error":"debug read-backs require an analytics backend that implements DebugReader (memory or clickhouse)"}`, http.StatusNotImplemented)
				return false
			}
			return true
		}

		mux.HandleFunc(routes.DebugAuctionWins, func(w http.ResponseWriter, r *http.Request) {
			if !memGuard(w) {
				return
			}
			traceID := r.URL.Query().Get("trace_id")
			bidModel := r.URL.Query().Get("bid_model")
			var count int
			if bidModel != "" {
				count = dbg.AuctionWinByBidModel(traceID, bidModel)
			} else {
				count = dbg.AuctionWinCount(traceID)
			}
			json.NewEncoder(w).Encode(map[string]int{"count": count})
		})

		mux.HandleFunc(routes.DebugBudgetDepletions, func(w http.ResponseWriter, r *http.Request) {
			if !memGuard(w) {
				return
			}
			campaignID := r.URL.Query().Get("campaign_id")
			json.NewEncoder(w).Encode(map[string]int{"count": dbg.BudgetDepletionsByCampaign(campaignID)})
		})

		// Committed-spend view + force-publish (backend-agnostic — reads the
		// billing engine's accumulator, not the analytics store, so no memGuard).
		// Guard the typed-nil interface pitfall: only hand over a bus when NATS
		// actually connected, else POST force-publish reports it couldn't send.
		var snapBus events.EventBus
		if natsBus != nil {
			snapBus = natsBus
		}
		mux.HandleFunc(routes.DebugSpendSnapshot, spendSnapshotDebugHandler(billingEngine, snapBus, clk, log))

		mux.HandleFunc(routes.DebugCampaignStateChanges, func(w http.ResponseWriter, r *http.Request) {
			if !memGuard(w) {
				return
			}
			campaignID := r.URL.Query().Get("campaign_id")
			json.NewEncoder(w).Encode(dbg.CampaignStateChangesByCampaign(campaignID))
		})

		mux.HandleFunc(routes.DebugRenderFailures, func(w http.ResponseWriter, r *http.Request) {
			if !memGuard(w) {
				return
			}
			creativeID := r.URL.Query().Get("creative_id")
			json.NewEncoder(w).Encode(dbg.RenderFailuresByCreative(creativeID))
		})

		mux.HandleFunc(routes.DebugFreqCapBlocks, func(w http.ResponseWriter, r *http.Request) {
			if !memGuard(w) {
				return
			}
			campaignID := r.URL.Query().Get("campaign_id")
			json.NewEncoder(w).Encode(dbg.FreqCapBlocksByCampaign(campaignID))
		})

		mux.HandleFunc(routes.DebugTrackerRejections, func(w http.ResponseWriter, r *http.Request) {
			if !memGuard(w) {
				return
			}
			traceID := r.URL.Query().Get("trace_id")
			reason := r.URL.Query().Get("reason")
			if traceID != "" {
				json.NewEncoder(w).Encode(dbg.TrackerRejectionsByTrace(traceID, reason))
				return
			}
			if reason != "" {
				json.NewEncoder(w).Encode(map[string]int{"count": dbg.TrackerRejectionsByReason(reason)})
				return
			}
			http.Error(w, `{"error":"trace_id or reason required"}`, http.StatusBadRequest)
		})

		mux.HandleFunc(routes.DebugServeNoFills, func(w http.ResponseWriter, r *http.Request) {
			if !memGuard(w) {
				return
			}
			traceID := r.URL.Query().Get("trace_id")
			json.NewEncoder(w).Encode(map[string]int{"count": dbg.ServeNoFillsByTrace(traceID)})
		})

		mux.HandleFunc(routes.DebugMediaEvents, func(w http.ResponseWriter, r *http.Request) {
			if !memGuard(w) {
				return
			}
			traceID := r.URL.Query().Get("trace_id")
			channel := r.URL.Query().Get("channel")
			eventType := r.URL.Query().Get("event_type")
			json.NewEncoder(w).Encode(map[string]int{"count": dbg.MediaEventsByTrace(traceID, channel, eventType)})
		})

		// Trigger a rollup run on demand (ops + e2e). Works on any backend
		// since it goes through the analytics.Store / RollupWriter interfaces.
		mux.HandleFunc(routes.DebugRollupRun, rollupRunHandler(rollupEngine, log)) // legacy alias

		// Routing warm-start source (ADR 0003): per-(channel, DSP) dsp_calls
		// aggregate the exchange fetches on boot to seed its SmartRouter.
		// ?since_hours=N (default 6). Works on any backend implementing
		// DSPCallAggregator (memory, clickhouse).
		mux.HandleFunc("/debug/routing/stats", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			agg, ok := store.(analytics.DSPCallAggregator)
			if !ok {
				http.Error(w, `{"error":"backend does not aggregate dsp_calls"}`, http.StatusNotImplemented)
				return
			}
			hours := 6
			if h := r.URL.Query().Get("since_hours"); h != "" {
				if n, err := strconv.Atoi(h); err == nil && n > 0 {
					hours = n
				}
			}
			since := time.Now().Add(-time.Duration(hours) * time.Hour)
			// since_ms (unix millis) overrides since_hours — the exchange's
			// router reseed passes its reset watermark here so a broadcast
			// reset genuinely forgets pre-reset history.
			if ms := r.URL.Query().Get("since_ms"); ms != "" {
				if n, err := strconv.ParseInt(ms, 10, 64); err == nil && n > 0 {
					since = time.UnixMilli(n)
				}
			}
			stats, err := agg.DSPCallStats(r.Context(), since)
			if err != nil {
				log.Error("dsp_call stats query failed", "error", err)
				http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
				return
			}
			json.NewEncoder(w).Encode(stats)
		})

		// Creative warm-start source (ADR 0003 part C): per-creative
		// impressions+clicks the ad server fetches on boot to seed its bandit.
		mux.HandleFunc("/debug/creative/stats", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			agg, ok := store.(analytics.CreativeStatAggregator)
			if !ok {
				http.Error(w, `{"error":"backend does not aggregate creative stats"}`, http.StatusNotImplemented)
				return
			}
			hours := 24
			if h := r.URL.Query().Get("since_hours"); h != "" {
				if n, err := strconv.Atoi(h); err == nil && n > 0 {
					hours = n
				}
			}
			stats, err := agg.CreativeStats(r.Context(), time.Now().Add(-time.Duration(hours)*time.Hour))
			if err != nil {
				log.Error("creative stats query failed", "error", err)
				http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
				return
			}
			json.NewEncoder(w).Encode(stats)
		})

		// Billing ledger reset — wipes in-memory ledger entries so e2e
		// billing tests can run in isolation. No-op on TigerBeetle backend
		// (the type assertion fails and we return reset=false).
		mux.HandleFunc(routes.DebugBillingReset, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			// Reset the pacing accumulator too (backend-agnostic in-memory state,
			// hydrated on boot) so committed-spend tests start empty.
			billingEngine.ResetPacing()
			if mem, ok := ledger.(*billing.MemoryLedger); ok {
				mem.Reset()
				json.NewEncoder(w).Encode(map[string]any{"reset": true, "backend": "memory", "pacing_reset": true})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"reset": false, "backend": "tigerbeetle", "pacing_reset": true})
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

	// Trace inspector: scoped + redacted per-request flow + a recent-impressions
	// drill-down for the portals. Scoping/redaction enforced in trace.go from the
	// gateway-injected X-Account-Type/-ID headers.
	mux.HandleFunc(routes.ReportingTrace, traceHandler(store, ledger, log))
	mux.HandleFunc(routes.ReportingRecentImpressions, recentImpressionsHandler(store, log))

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

	// dealTypeFor resolves a beacon deal id to its deal_type so contract
	// fee modifiers (keyed by TYPE) actually match. Defaults to "" (open
	// market) when unset. See dealtypes.go.
	dealTypeFor func(ctx context.Context, dealID string) string

	// Batch-consumer path (opt-in via reporting.clickhouse_batch_consumer).
	// When enabled, the high-volume core subjects are consumed in bulk (one
	// atomic block insert per fetch) with per-message dedup; see batch.go.
	batch        analytics.BatchInserter
	dedup        events.DedupStore
	dedupTTL     time.Duration
	batchEnabled bool

	// dataFee settles parked data-monetization attribution at impression
	// time (datafee.go). nil = feature off (no Postgres) — all hooks no-op.
	dataFee *dataFeeAccrual

	// marketplace settles data-marketplace CPM surcharges at impression time
	// (marketplace.go). nil = feature off — hooks no-op.
	marketplace *marketplaceAccrual

	// viewThrough credits click-less conversions to a prior viewable exposure
	// (attribution.go). nil = not wired → Phase-0 (deterministic ctid) behaviour
	// only, and settle is never gated.
	viewThrough *viewThroughAttributor
}

// SetDataFeeAccrual connects data-monetization accrual (nil-tolerant).
func (c *EventConsumer) SetDataFeeAccrual(a *dataFeeAccrual) { c.dataFee = a }

// SetMarketplaceAccrual connects data-marketplace surcharge settlement (nil-tolerant).
func (c *EventConsumer) SetMarketplaceAccrual(a *marketplaceAccrual) { c.marketplace = a }

// SetViewThroughAttributor wires view-through conversion attribution (nil-tolerant).
func (c *EventConsumer) SetViewThroughAttributor(a *viewThroughAttributor) { c.viewThrough = a }

// attributionEnabled reports whether conversion attribution + settle is on.
// When the attributor isn't wired it defaults ON (Phase-0 deterministic path).
func (c *EventConsumer) attributionEnabled() bool {
	if c.viewThrough == nil {
		return true
	}
	return c.viewThrough.enabled()
}

// attributeConversion gives a click-less conversion its view-through credit in
// place (no-op when already click-through attributed or the attributor is off).
func (c *EventConsumer) attributeConversion(ctx context.Context, e *analytics.ConversionEvent) {
	if c.viewThrough != nil {
		c.viewThrough.attribute(ctx, e)
	}
}

// handleDataFee parks one DataFeeEvent for its impression (datafee.go).
func (c *EventConsumer) handleDataFee(ctx context.Context, msg *events.Message) error {
	return c.dataFee.HandleEvent(ctx, msg)
}

func NewEventConsumer(log *slog.Logger, store analytics.Store, billingEngine *billing.Engine) *EventConsumer {
	return &EventConsumer{log: log, store: store, billing: billingEngine}
}

// SetDealTypeResolver wires deal-id -> deal_type resolution for billing
// spend events (contract deal-type fee modifiers).
func (c *EventConsumer) SetDealTypeResolver(fn func(ctx context.Context, dealID string) string) {
	c.dealTypeFor = fn
}

// resolveDealType is nil-safe: no resolver = open-market ("").
func (c *EventConsumer) resolveDealType(ctx context.Context, dealID string) string {
	if c.dealTypeFor == nil {
		return ""
	}
	return c.dealTypeFor(ctx, dealID)
}

// EnableBatchConsumer turns on the bulk NATS consumer path for core events.
// batch is the store's bulk-insert capability; dedup makes redelivery safe.
func (c *EventConsumer) EnableBatchConsumer(batch analytics.BatchInserter, dedup events.DedupStore, ttl time.Duration) {
	c.batch = batch
	c.dedup = dedup
	c.dedupTTL = ttl
	c.batchEnabled = true
}

// RegisterNATSSubscriptions sets up NATS consumers for all event subjects.
// Called when NATS is available.
func (c *EventConsumer) RegisterNATSSubscriptions(bus events.EventBus) error {
	subjects := map[string]events.Handler{
		events.SubjectImpression:             c.handleImpression,
		events.SubjectClick:                  c.handleClick,
		events.SubjectConversion:             c.handleConversion,
		events.SubjectView:                   c.handleView,
		events.SubjectAuctionComplete:        c.handleAuction,
		events.SubjectAuctionWin:             c.handleAuctionWin,
		events.SubjectAuctionLoss:            c.handleAuctionLoss,
		events.SubjectDirectWin:              c.handleDirectWin,
		events.SubjectPrebidOutboundWin:      c.handlePrebidOutboundWin,
		events.SubjectBudgetDepleted:         c.handleBudgetDepleted,
		events.SubjectCampaignStateChanged:   c.handleCampaignState,
		events.SubjectTrackerRejected:        c.handleTrackerRejected,
		events.SubjectAdserverRenderFailed:   c.handleRenderFailed,
		events.SubjectAdserverFreqCapBlocked: c.handleFreqCapBlocked,
		events.SubjectVideo:                  c.handleVideo,
		events.SubjectAudio:                  c.handleAudio,
		events.SubjectServeNoFill:            c.handleServeNoFill,
		events.SubjectDSPCall:                c.handleDSPCall,
		// Profile-store tables (ADR 0006 phase 1). Registered here too so the
		// data still lands in ClickHouse when the batch consumer is disabled;
		// when it IS enabled these are removed below and served in bulk via
		// coreBatchHandlers. Same NATSGroupReporting group either way — reporting
		// fans out independently of the pipeline's lake sink (untouched).
		events.SubjectBehaviourObserved: c.handleBehaviourSignal,
		events.SubjectProfileSignal:     c.handleProfileSignal,
		// Data monetization (ADR 0009): park the SSP's attribution record
		// until the impression for its trace arrives (see datafee.go).
		// Low-volume operational subject — always per-message.
		events.SubjectDataFee: c.handleDataFee,
	}

	ctx := context.Background()

	// Build the full subscribe work-list. Batch path: when enabled and the bus
	// supports it, the high-volume core subjects are consumed in bulk (one
	// atomic block insert per fetch) and removed from the per-message map so
	// they aren't double-subscribed. Operational-signal subjects stay per-message.
	// Each work item carries its own typed subscribe call (batch handlers are a
	// distinct type from per-message handlers), so both paths share one retry.
	type sub struct {
		subject   string
		subscribe func() error
	}
	var work []sub
	batchSub, batchOK := bus.(events.BatchSubscriber)
	if batchOK && c.batchEnabled && c.batch != nil && c.dedup != nil {
		for subject, handler := range c.coreBatchHandlers() {
			subject, handler := subject, handler
			work = append(work, sub{subject, func() error {
				return batchSub.SubscribeBatch(ctx, subject, constants.NATSGroupReporting, handler)
			}})
			delete(subjects, subject)
		}
	}
	for subject, handler := range subjects {
		subject, handler := subject, handler
		work = append(work, sub{subject, func() error {
			return bus.Subscribe(ctx, subject, constants.NATSGroupReporting, handler)
		}})
	}

	// Self-heal, don't latch: a subscribe that loses the NATS/JetStream boot
	// race must RETRY, not go deaf — otherwise reporting silently consumes
	// nothing and NO events land in ClickHouse. Retry only the failed subjects
	// every 15s until they stick (same doctrine as webhooks/notifications).
	attempt := func(items []sub) []sub {
		var pending []sub
		for _, s := range items {
			if err := s.subscribe(); err != nil {
				c.log.Error("subscribe failed, will retry", "subject", s.subject, "error", err)
				pending = append(pending, s)
			} else {
				c.log.Info("subscribed to NATS subject", "subject", s.subject)
			}
		}
		return pending
	}
	if pending := attempt(work); len(pending) > 0 {
		go func() {
			for len(pending) > 0 {
				time.Sleep(15 * time.Second)
				pending = attempt(pending)
			}
		}()
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

	// e.ClearingPriceUSD is already the realized per-impression cost — the
	// tracker converts the auction CPM at the source (see cmd/tracker), so hot,
	// cold, and the ledger all book the same per-impression dollars.
	if err := c.store.InsertImpression(ctx, &e); err != nil {
		c.log.Error("failed to write impression", "error", err, "trace_id", e.TraceID)
		return msg.Nak()
	}

	// Bill atomically with analytics write (unified consumer)
	if c.billing != nil {
		c.billing.ProcessEvent(ctx, billing.SpendEvent{
			TraceID: e.TraceID, CampaignID: e.CampaignID, CreativeID: e.CreativeID,
			PlacementID: e.PlacementID, PublisherID: e.PublisherID, AdvertiserID: e.AccountID,
			ClearingPrice: e.ClearingPriceUSD, Currency: spendCurrency(e.ClearingCurrency),
			BidModel: billing.BidModel(e.BidModel), DealType: c.resolveDealType(ctx, e.DealID),
			EventType: "impression", Timestamp: e.Timestamp,
		})
	}

	// Data monetization: settle any parked fee attribution for this trace
	// (single PK lookup; near-always a miss). Never blocks the Ack — the
	// impression is already recorded and billed.
	c.dataFee.AccrueOnImpression(ctx, e.TraceID)
	c.marketplace.AccrueOnImpression(ctx, &e)

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

	// Attribute BEFORE the insert so the stored row records the linkage. Click-
	// through (deterministic ctid) is already stamped by the tracker; this fills
	// in view-through for click-less conversions.
	c.attributeConversion(ctx, &e)

	if err := c.store.InsertConversion(ctx, &e); err != nil {
		c.log.Error("failed to write conversion", "error", err, "trace_id", e.TraceID)
		return msg.Nak()
	}

	// Settle the CPA reservation (if one exists). No-op for other models.
	// Attribution closes the loop: the reservation lives on the EARNING
	// exposure's trace (the impression/click), not the conversion's own trace,
	// so settle against SettleTraceID (AttributedTraceID when set). Gated by the
	// attribution kill-switch.
	if c.billing != nil && c.attributionEnabled() {
		if _, err := c.billing.SettleByTrace(ctx, e.SettleTraceID(), "conversion"); err != nil {
			c.log.Warn("conversion settle failed", "trace_id", e.SettleTraceID(), "error", err)
		}
	}

	c.log.Debug("conversion recorded", "trace_id", e.TraceID,
		"attributed_trace_id", e.AttributedTraceID, "attribution_type", e.AttributionType,
		"campaign_id", e.CampaignID)
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
		CampaignID: src.CampaignID, CreativeID: src.CreativeID,
		PlacementID: src.PlacementID, PublisherID: src.PublisherID,
		AccountID: src.AccountID, Timestamp: src.Timestamp,
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
		CampaignID: src.CampaignID, CreativeID: src.CreativeID,
		PlacementID: src.PlacementID, PublisherID: src.PublisherID,
		AccountID: src.AccountID, Timestamp: src.Timestamp,
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
	if obs, ok := c.store.(analytics.ObservabilityWriter); ok {
		obs.InsertServeNoFill(analytics.ServeNoFill{
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
	if obs, ok := c.store.(analytics.ObservabilityWriter); ok {
		obs.InsertFreqCapBlock(analytics.FreqCapBlock{
			TraceID:     src.TraceID,
			UserID:      src.UserID,
			HouseholdID: src.HouseholdID,
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
	if obs, ok := c.store.(analytics.ObservabilityWriter); ok {
		obs.InsertRenderFailure(analytics.RenderFailure{
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
	if obs, ok := c.store.(analytics.ObservabilityWriter); ok {
		obs.InsertTrackerRejection(analytics.TrackerRejection{
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
	if obs, ok := c.store.(analytics.ObservabilityWriter); ok {
		obs.InsertCampaignStateChange(analytics.CampaignStateChange{
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
	if obs, ok := c.store.(analytics.ObservabilityWriter); ok {
		obs.InsertBudgetDepletion(analytics.BudgetDepletion{
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

// handleAuctionLoss lands the durable per-advertiser loss record (counterpart
// to handleAuctionWin). It is what makes the advertiser bid-shading view's
// win/loss survive a DSP redeploy — losses used to live only in the DSP's
// in-memory tracker. Counts only, so at-least-once redelivery is fine.
func (c *EventConsumer) handleAuctionLoss(ctx context.Context, msg *events.Message) error {
	var src events.AuctionLossEvent
	if err := json.Unmarshal(msg.Data, &src); err != nil {
		c.log.Error("failed to decode auction loss event", "error", err)
		return msg.Ack() // bad payload — don't redeliver
	}
	if src.Timestamp.IsZero() {
		src.Timestamp = time.Now()
	}
	e := analytics.AuctionLossEvent{
		TraceID:       src.TraceID,
		AuctionID:     src.AuctionID,
		AccountID:     src.AccountID,
		CampaignID:    src.CampaignID,
		PlacementID:   src.PlacementID,
		ClearingPrice: src.ClearingPrice,
		LossReason:    src.LossReason,
		Channel:       src.Channel,
		SchemaVersion: 1,
		Timestamp:     src.Timestamp,
	}
	if err := c.store.InsertAuctionLoss(ctx, &e); err != nil {
		c.log.Error("failed to write auction loss", "error", err, "placement_id", e.PlacementID)
		return msg.Nak()
	}
	return msg.Ack()
}

// handleDSPCall lands per-DSP routing telemetry (ADR 0003) on the per-message
// path (the batch path uses handleDSPCallBatch). Any backend that implements
// BatchInserter (memory/duckdb/clickhouse) stores it; others drop it.
func (c *EventConsumer) handleDSPCall(ctx context.Context, msg *events.Message) error {
	var src events.DSPCallEvent
	if err := json.Unmarshal(msg.Data, &src); err != nil {
		c.log.Error("failed to decode dsp_call event", "error", err)
		return msg.Ack()
	}
	if src.Timestamp.IsZero() {
		src.Timestamp = time.Now()
	}
	bi, ok := c.store.(analytics.BatchInserter)
	if !ok {
		return msg.Ack() // backend can't store dsp_calls; drop silently
	}
	e := analytics.DSPCallEvent{
		TraceID: src.TraceID, AuctionID: src.AuctionID, Channel: src.Channel,
		DSPEndpoint: src.DSPEndpoint, BidReceived: src.BidReceived, BidPriceUSD: src.BidPriceUSD,
		LatencyMs: src.LatencyMs, TimedOut: src.TimedOut, SchemaVersion: 1, Timestamp: src.Timestamp,
	}
	if err := bi.InsertDSPCalls(ctx, []*analytics.DSPCallEvent{&e}); err != nil {
		c.log.Error("failed to write dsp_call", "error", err, "trace_id", e.TraceID)
		return msg.Nak()
	}
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
						Currency: spendCurrency(imp.ClearingCurrency), BidModel: billing.BidModel(imp.BidModel),
						EventType: "impression", Timestamp: imp.Timestamp,
					})
				}
			}
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

// queryHandler serves analytics queries via HTTP.
// querier is the read surface queryHandler needs — satisfied by both a raw
// analytics.Store and the reporting.QueryEngine wrapping it.
type querier interface {
	Query(ctx context.Context, params analytics.QueryParams) (*analytics.QueryResult, error)
}

// contractNet adapts the warm ContractStore to reporting.NetResolver so the
// metrics engine computes net_revenue without pkg/reporting importing pkg/billing.
type contractNet struct{ store *billing.ContractStore }

func (c contractNet) Net(publisherID string, gross float64) float64 {
	return c.store.Get(publisherID).CalculateRevenue(gross, "").PublisherRevenue
}

func queryHandler(log *slog.Logger, q querier, queryTimeout func() time.Duration) http.HandlerFunc {
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

		// Queries can legitimately outlive the server-wide 30s WriteTimeout —
		// a deep-history read spans the cold Parquet lake (async report jobs
		// depend on this). Extend the deadline for THIS response only; every
		// other reporting route keeps the tight server default.
		timeout := queryTimeout()
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(timeout)); err != nil {
			log.Warn("query write-deadline extension unsupported", "error", err)
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()

		result, err := q.Query(ctx, params)
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
// monthStartUTC returns 00:00 on the 1st of now's UTC calendar month — the
// window start for a publisher's month-to-date impression count (tier reset
// boundary), matching how the billing day/month boundaries roll on UTC.
func monthStartUTC(now time.Time) time.Time {
	u := now.UTC()
	return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func startContractCache(cfg *config.Config, clk clock.Clock, log *slog.Logger, contracts *billing.ContractStore, impReader analytics.PublisherImpressionReader) *warm.Cache[postgres.ContractRow] {
	pollInterval := firstNonZeroDuration(
		cfg.GetDuration(keys.Reporting.WarmBillingRatesPollInterval.Key(), 0),
		cfg.GetDuration(keys.CacheWarm.PollInterval.Key(), 300*time.Second),
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
			anyTiered := false
			for _, r := range rows {
				contracts.Set(r.PublisherID, r.Contract)
				if r.Contract != nil && (r.Contract.Model == billing.ModelTiered || r.Contract.Model == billing.ModelHybrid) {
					anyTiered = true
				}
			}
			// Tiered revenue share picks the fee off month-to-date impressions —
			// refresh that cluster-global count from analytics. Skip the query
			// entirely when no publisher is on a volume tier (the common case).
			if anyTiered && impReader != nil {
				qctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				since := monthStartUTC(clk.Now())
				if counts, err := impReader.ImpressionsByPublisher(qctx, since); err != nil {
					log.Warn("tiered revshare: month-impression refresh failed", "error", err)
				} else {
					for pub, n := range counts {
						contracts.SetMonthImpressions(pub, n)
					}
				}
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
	dbURL := cfg.Get(keys.Database.URL.Key(), "")
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
	url := keys.Reporting.NATSURL.Get(cfg)
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

// spendCurrency returns the impression's REAL clearing currency for the
// billing engine ("USD" when unstamped). The old hardcoded "USD" bypassed
// billing's exchange-rate normalization entirely — a EUR impression was
// booked 1:1 as dollars (caught by TestBillingCurrencyConversion).
func spendCurrency(cur string) string {
	if cur == "" {
		return "USD"
	}
	return cur
}
