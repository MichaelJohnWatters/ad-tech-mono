// cmd/exchange is the Ad Exchange service.
// Receives bid requests, fans out to DSPs, runs auctions.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auction"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache/warm"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/deals"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events/natsbus"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/fraud"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/health"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/identityobserve"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/optimise"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/secrets"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
	_ "github.com/lib/pq"
	"go.opentelemetry.io/otel/attribute"
)

func main() {
	clk := clock.Real{}
	log := logger.New(constants.ServiceExchange)
	sc := config.Setup(constants.ServiceExchange, keys.ExchangeSchema(), log)
	cfg := sc.Cfg
	hlth := health.New()
	lc := lifecycle.New(log)

	// OpenTelemetry — exporter ships to Jaeger via OTLP/HTTP. Empty endpoint
	// disables tracing entirely so dev/test envs without Jaeger still boot.
	otelShutdown := tracing.Init(context.Background(), tracing.Config{
		ServiceName:    constants.ServiceExchange,
		ServiceVersion: keys.Otel.ServiceVersion.Get(cfg),
		Endpoint:       keys.Otel.Endpoint.Get(cfg),
		SampleRatio:    keys.Otel.SampleRatio.Get(cfg),
		Log:            log,
	})
	lc.OnShutdown("otel", func(ctx context.Context) error { return otelShutdown(ctx) })

	knobs := NewKnobs(sc)
	port := keys.Exchange.Port.Get(cfg)
	// dspEndpointsFn reads the in-memory config map per auction (cheap;
	// it's a sync.RWMutex-guarded map lookup, not a Postgres query). The
	// freshness story: the config manager polls Postgres every 30s and
	// also re-polls on adtech.cache.invalidate.config NATS messages
	// (broadcast by SetConfigForPod / PUT /v1/config). So a config edit
	// lands in this map within NATS round-trip time, and every subsequent
	// auction sees the new value. Empty entries are filtered so a trailing
	// comma doesn't introduce a phantom endpoint.
	dspEndpointsFn := func() []string {
		raw := keys.Exchange.DSPEndpoints.Get(cfg)
		parts := strings.Split(raw, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
		return out
	}

	engine := auction.NewEngine(clk)
	// httpClient.Timeout is the belt-and-braces fallback if the per-request
	// context deadline doesn't fire. Live-updated on bid_timeout edits so
	// the value never drifts from the auction-handler context deadline.
	httpClient := &http.Client{Timeout: knobs.BidTimeout.Value()}
	sc.Manager.OnChange(keys.Exchange.BidTimeout.Key(), func(_, _, newVal string) {
		httpClient.Timeout = knobs.BidTimeout.Value()
		log.Info("bid timeout updated live", "new", newVal)
	})
	adsTxtCache := fraud.NewAdsTxtCache()

	// Connect to NATS for auction event publishing
	natsURL := keys.Exchange.NATSURL.Get(cfg)
	var pub *events.Publisher
	natsBus, err := natsbus.New(natsURL, constants.ServiceExchange, log)
	if err != nil {
		log.Warn("nats unavailable, auction events will not be published", "error", err)
	} else {
		ctx := context.Background()
		natsBus.EnsureStream(ctx, events.StreamName, []string{events.StreamSubjects})
		pub = events.NewPublisher(natsBus, log)
		lc.OnShutdown("nats", func(_ context.Context) error { return natsBus.Close() })
	}

	// Identity auto-build: publish observed identifiers from inbound Prebid
	// requests (external demand our SSP never saw) to the identity-consumer.
	var idPub *identityobserve.Publisher
	if keys.Exchange.IdentityObserveEnabled.Get(cfg) && natsBus != nil {
		idPub = identityobserve.NewPublisher(natsBus, log)
		log.Info("exchange identity observation enabled (prebid inbound)")
	}

	metrics := middleware.NewMetrics(constants.ServiceExchange)
	// Auction-domain counters (adtech_auctions_total, revenue, …) emitted
	// from the same /metrics endpoint as the generic HTTP metrics.
	auctionM := newAuctionMetrics(metrics.Registry())

	// Warm cache of active deals.
	dealCache := startDealCache(cfg, clk, log)
	if dealCache != nil {
		lc.OnShutdown("deal-cache", func(_ context.Context) error { dealCache.Stop(); return nil })
	}

	// ads.txt seller-authorisation cache + per-auction enforcement gate.
	// The gate reads exchange.adstxt_enforcement live (off by default), so
	// dev/e2e are unaffected unless explicitly switched to warn/strict.
	var adsTxtBus events.EventBus
	if natsBus != nil {
		adsTxtBus = natsBus
	}
	adsTxtWarm := startAdsTxtCache(cfg, log, adsTxtBus, adsTxtCache)
	if adsTxtWarm != nil {
		lc.OnShutdown("adstxt-cache", func(_ context.Context) error { adsTxtWarm.Stop(); return nil })
	}
	adsTxtGate := adsTxtGateFn(cfg, adsTxtCache, log)
	schainGate := schainGateFn(cfg, log)
	// ads.cert signing: the ACTIVE Ed25519 key comes from the secrets store
	// (purpose adcert_ed25519) so it rotates HOT with overlap (Phase I); the
	// /v1/adcert/key endpoint publishes the whole non-revoked keyset. Falls back
	// to exchange.adcert_sign_key (env) until a secret is configured.
	adcertSecrets := secrets.Start(context.Background(), cfg, clk, log, constants.ServiceExchange)
	lc.OnShutdown("exchange-secrets-cache", func(_ context.Context) error { adcertSecrets.Stop(); return nil })
	adcertSigner := newAdCertSigner(cfg, adcertSecrets, clk.Now, log)
	adcertSecrets.WatchActive(context.Background(), secrets.PurposeAdCertEd25519, 30*time.Second, adcertSigner.setActiveKey)
	signReq := adcertSigner.sign

	// Readiness: deal cache must have run at least once (an empty result
	// is still "ready" — empty is a valid state for fresh seed). Exchange
	// can run open auctions without deals, so the cache being available
	// is sufficient.
	hlth.AddReadinessCheck("deal-cache", func(_ context.Context) error {
		ts, err := dealCache.LastLoaded()
		if err != nil {
			return err
		}
		if ts.IsZero() {
			return errors.New("deal cache not yet loaded")
		}
		return nil
	})

	// Smart router: tracks per-DSP bid/win/timeout history; the auction
	// handler asks it to filter the fan-out list each request so we stop
	// calling DSPs that haven't bid in a long time.
	router := optimise.NewSmartRouter()
	routerMinCalls := keys.Exchange.RoutingMinCalls.Get(cfg)
	_ = routerMinCalls // SmartRouter uses 20 as a hardcoded threshold today
	// Warm-start routing from reporting's dsp_calls history (ADR 0003) so a
	// restarted exchange isn't cold. Async + fail-open — never blocks boot.
	go warmStartRouter(cfg, router, log)

	mux := http.NewServeMux()
	mux.Handle(routes.Healthz, hlth.LivenessHandler())
	mux.Handle(routes.Readyz, hlth.ReadinessHandler())
	mux.Handle(routes.Metrics, metrics.Handler())

	// Debug surface — all behind debug.endpoints_enabled (default true in
	// dev, expected false in prod overlays). Reads + mutations both gated
	// so the prod surface is purely the auction/win/loss/Prebid paths.
	if keys.Debug.EndpointsEnabled.Get(cfg) {
		refreshables := []warm.Refreshable{dealCache}
		if adsTxtWarm != nil {
			refreshables = append(refreshables, adsTxtWarm)
		}
		mux.HandleFunc(routes.DebugCacheRefresh, warm.RefreshHandler(refreshables...))

		// Dump the active deal warm-cache snapshot.
		mux.HandleFunc(routes.DebugExchangeDeals, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			json.NewEncoder(w).Encode(dealCache.All())
		})

		// Smart router inspector. Query params:
		//   ?preview=true                 — returns the filtered DSP list
		//                                   SelectDSPs would pick right now
		//                                   (read-only).
		//   ?preview=true&channel=display — preview against a specific channel.
		//   ?reset=true                   — clears the router's learned stats.
		//                                   Used by e2e tests for a deterministic
		//                                   training baseline.
		//   (no params)                   — full stats list (default).
		mux.HandleFunc(routes.DebugExchangeRouting, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			q := r.URL.Query()
			if q.Get("reset") == "true" {
				router.Reset()
				json.NewEncoder(w).Encode(map[string]any{"reset": true})
				return
			}
			if q.Get("preview") == "true" {
				channel := q.Get("channel")
				if channel == "" {
					// Routing stats are keyed per request format. A multi-format
					// exchange (exchange.channel="all") never accumulates stats
					// under "all", so previewing it would show a stale/empty
					// state — fall to the dominant format. A channel-restricted
					// exchange (video/native/…) keeps its configured channel,
					// which is exactly what its traffic records under.
					channel = knobs.Channel()
					if channel == constants.ChannelAll {
						channel = constants.ChannelDisplay
					}
				}
				selected := router.Preview(channel, dspEndpointsFn())
				json.NewEncoder(w).Encode(map[string]any{
					"channel":  channel,
					"all":      dspEndpointsFn(),
					"selected": selected,
				})
				return
			}
			json.NewEncoder(w).Encode(router.Stats())
		})
	}

	// Auction handler reads bid_timeout via knobs.BidTimeout per request so
	// UI edits land without a restart (also used as the fan-out context
	// deadline).
	debugEnabledFn := func() bool { return keys.Debug.EndpointsEnabled.Get(cfg) }
	// Per-auction decision to emit DSP-call telemetry: gated on/off, then
	// sampled by a deterministic hash of trace_id so either all or none of an
	// auction's per-DSP events fire (keeps the win-rate join consistent) and a
	// high-QPS exchange can throttle background NATS volume without losing the
	// signal. Both knobs are live-tunable.
	emitDSPCallFn := func(traceID string) bool {
		if !keys.Exchange.EmitDSPCallEvents.Get(cfg) {
			return false
		}
		ratio := keys.Exchange.DSPCallSampleRatio.Get(cfg)
		if ratio >= 1.0 {
			return true
		}
		if ratio <= 0 {
			return false
		}
		return sampleTrace(traceID, ratio)
	}
	auction := auctionHandler(log, clk, engine, httpClient, knobs.BidTimeout.Value, dspEndpointsFn, knobs.Channel, debugEnabledFn, pub, adsTxtCache, adsTxtGate, schainGate, signReq, dealCache, router, auctionM, emitDSPCallFn)
	mux.HandleFunc(routes.OpenRTBAuction, auction)
	mux.HandleFunc(routes.AdCertKey, adCertKeyHandler(adcertSigner))
	mux.HandleFunc(routes.OpenRTBWin, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc(routes.OpenRTBLoss, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })

	// Prebid Server-compatible bidder endpoint. See pkg/prebid + docs/PLAN.md
	// → "Prebid Server Integration". Reuses the same auction path with the
	// inbound floor policy + opaque deal-id logging applied first.
	mux.HandleFunc(routes.PrebidAuction, prebidAuctionHandler(cfg, auction, idPub, log))
	mux.HandleFunc(routes.PrebidSetUID, prebidSetUIDHandler(log))

	handler := tracing.HTTPMiddleware(constants.ServiceExchange)(metrics.Wrap(middleware.CORS(mux)))

	server := &http.Server{Addr: ":" + port, Handler: handler, ReadTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second}

	log.Info("exchange starting", "port", port, "channel", knobs.Channel(), "dsps", dspEndpointsFn())
	lifecycle.ServeHTTP(lc, server, log, 30*time.Second)
}

func startDealCache(cfg *config.Config, clk clock.Clock, log *slog.Logger) *warm.Cache[models.Deal] {
	pollInterval := firstNonZeroDuration(
		cfg.GetDuration(keys.Exchange.WarmDealsPollInterval.Key(), 0),
		keys.CacheWarm.PollInterval.Get(cfg),
	)
	loader := pickDealLoader(cfg, log)
	bus := connectInvalidateBus(cfg, log)
	c := warm.New(warm.Config[models.Deal]{
		Name:              "deals",
		Loader:            loader,
		Clock:             clk,
		Bus:               bus,
		InvalidateSubject: events.SubjectCacheInvalidateDeals,
		PollInterval:      pollInterval,
		Log:               log,
	})
	if err := c.Start(context.Background()); err != nil {
		log.Warn("deal cache initial load failed (continuing with empty set)", "error", err)
	}
	return c
}

// pickDealLoader returns a self-healing warm.Loader. The underlying
// PostgresLoader is lazy-constructed on first LoadAll and re-constructed
// after any error, so a service that boots before Postgres is reachable
// will pick up rows automatically on the next 30s poll instead of being
// pinned to an empty cache for its lifetime.
func pickDealLoader(cfg *config.Config, log *slog.Logger) warm.Loader[models.Deal] {
	dbURL := cfg.Get(keys.Database.URL.Key(), "")
	return &warm.RetryingLoader[models.Deal]{
		Log:   log,
		KeyFn: func(d models.Deal) string { return d.ID },
		Construct: func() (warm.Loader[models.Deal], error) {
			if dbURL == "" {
				return nil, fmt.Errorf("database.url not set")
			}
			store, err := postgres.New(postgres.Config{PrimaryURL: dbURL, MaxOpenConns: 5, MaxIdleConns: 2, ConnMaxLifetime: 5 * time.Minute})
			if err != nil {
				return nil, fmt.Errorf("postgres connect: %w", err)
			}
			return &postgres.DealLoader{Store: store}, nil
		},
	}
}

// connectInvalidateBus returns a NATS bus used only for cache invalidates.
// The auction publisher uses its own bus instance already; this one stays
// scoped to the warm cache so its lifecycle is independent.
func connectInvalidateBus(cfg *config.Config, log *slog.Logger) events.EventBus {
	url := keys.Exchange.NATSURL.Get(cfg)
	bus, err := natsbus.New(url, constants.ServiceExchange+"-cache", log)
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
	return 30 * time.Second
}

func auctionHandler(log *slog.Logger, clk clock.Clock, engine *auction.Engine, client *http.Client, bidTimeoutFn func() time.Duration, dspEndpointsFn func() []string, channelFn func() string, debugEnabledFn func() bool, pub *events.Publisher, adsTxt *fraud.AdsTxtCache, adsTxtGate func(string) (bool, string), schainGate func(*openrtb.BidRequest, string) (bool, string), signReq func(*openrtb.BidRequest), dealCache *warm.Cache[models.Deal], router *optimise.SmartRouter, am *auctionMetrics, emitDSPCallFn func(traceID string) bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Read the live channel knob once per request so UI edits to
		// exchange.channel apply on the next auction without a restart.
		channel := channelFn()

		var bidReq openrtb.BidRequest
		if err := json.NewDecoder(r.Body).Decode(&bidReq); err != nil {
			http.Error(w, "invalid bid request", http.StatusBadRequest)
			return
		}

		// Smart routing keys on the REQUEST's actual format, not the static
		// exchange.channel knob. Routing on "all" averages every format's bid
		// rate together, so a DSP that only has (say) native demand gets
		// starved out once display auctions drag its blended bid rate below
		// the drop threshold — and native/audio then never fill. Per-format
		// stats keep each DSP eligible for the formats it actually bids.
		routingChannel := channelForRequest(&bidReq)

		// W3C trace ID from the OTel span HTTPMiddleware created (or
		// extracted from the inbound traceparent header if the SSP propagated
		// one). Falls back to bidReq.ID for callers that still send their
		// own opaque ID (legacy simulator, e2e harness). Same string flows
		// into logs, NATS events, analytics, billing — single ID end-to-end.
		traceID := tracing.TraceIDFromContext(r.Context())
		if traceID == "" {
			traceID = bidReq.ID
		}
		ctx := logger.WithTraceID(r.Context(), traceID)
		reqLog := logger.WithContext(log, ctx)
		start := clk.Now()

		// ads.txt seller-authorisation gate (before fan-out): if the
		// publisher published an ads.txt that doesn't list us, strict mode
		// rejects the request outright. Off by default; no-ops when the
		// domain is unknown/unverifiable.
		if bidReq.Site != nil {
			if allow, reason := adsTxtGate(bidReq.Site.Domain); !allow {
				w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
				json.NewEncoder(w).Encode(openrtb.BidResponse{ID: bidReq.ID, NoBid: true})
				reqLog.Info("auction rejected", "reason", reason, "domain", bidReq.Site.Domain)
				am.auctionsTotal.WithLabelValues("rejected_adstxt", channel).Inc()
				return
			}
		}

		// SupplyChain (schain) gate: verify the transparency chain is present and
		// well-formed before fan-out. Strict mode no-bids invalid chains; warn
		// logs and allows. May append the exchange node when configured. Reads
		// enforcement live via schainGate.
		if allow, reason := schainGate(&bidReq, traceID); !allow {
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			json.NewEncoder(w).Encode(openrtb.BidResponse{ID: bidReq.ID, NoBid: true})
			reqLog.Info("auction rejected", "reason", reason)
			am.auctionsTotal.WithLabelValues("rejected_schain", channel).Inc()
			return
		}

		// ads.cert: sign the request (after any schain append) so DSPs can
		// verify it authentically came from this exchange. No-op when signing
		// is unconfigured.
		signReq(&bidReq)

		// Application-level span — the HTTP middleware already opened a
		// server span around the request, but we want the auction phases
		// (fanout, deal-eval, win-loss) to show as named children in Jaeger.
		ctx, auctionSpan := tracing.StartSpan(ctx, "exchange.auction",
			attribute.String("auction.trace_id", traceID),
			attribute.String("auction.channel", channel),
		)
		defer auctionSpan.End()

		// Smart routing: filter the DSP list to the ones likely to bid.
		// First-time/unseen DSPs get a neutral score and stay in. Heavy no-bid
		// or timeout patterns drop a DSP for this auction (still gets occasional
		// traffic via the periodic poll model — see optimise.SmartRouter).
		dspEndpoints := dspEndpointsFn()
		selectedEndpoints := router.SelectDSPs(routingChannel, dspEndpoints)
		if len(selectedEndpoints) == 0 {
			// Safety floor: if the router would skip everyone (cold start edge
			// case or learned-bad state), fall back to the full list. We never
			// want to silently no-bid because of routing.
			selectedEndpoints = dspEndpoints
		}

		reqLog.Info("auction started", "channel", routingChannel, "num_dsps_total", len(dspEndpoints), "num_dsps_called", len(selectedEndpoints))

		// Fan out to DSPs in parallel. The fan-out context carries the
		// bid_timeout deadline so:
		//   1. Slow DSPs whose HTTP call would exceed bid_timeout get cancelled
		//      via context propagation (client.Timeout is the belt-and-braces
		//      backstop for the same).
		//   2. The gather loop in fanOutToDSPs uses ctx.Done() to early-finish
		//      once the deadline elapses — auctions complete on the configured
		//      bid_timeout, not on "slowest DSP's actual response time."
		bidTimeout := bidTimeoutFn()
		fanCtx, fanCancel := context.WithTimeout(ctx, bidTimeout)
		defer fanCancel()
		fanCtx, fanSpan := tracing.StartSpan(fanCtx, "exchange.fanout",
			attribute.Int("dsps.called", len(selectedEndpoints)),
			attribute.Int64("bid_timeout_ms", bidTimeout.Milliseconds()),
		)
		// Dev-mode: publisher simulator can send a CSV of DSP indexes that
		// should be deliberately slow this auction, so the UI can demo the
		// timeout / early-finish behavior. Header is opt-in per request;
		// gated by debug.endpoints_enabled (read via debugEnabledFn) so
		// prod ignores it even if an upstream forwarded it. Empty value
		// means "all DSPs run normally."
		var slowDSPs map[int]bool
		if debugEnabledFn() {
			slowDSPs = parseSlowDSPs(r.Header.Get("X-Dev-Slow-DSPs"))
		}

		bids, bidRecords := fanOutToDSPs(fanCtx, client, selectedEndpoints, bidReq, routingChannel, slowDSPs, reqLog, router, pub, traceID, emitDSPCallFn(traceID))
		fanSpan.SetAttributes(attribute.Int("bids.received", len(bids)))
		fanSpan.End()

		if len(bids) == 0 {
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			json.NewEncoder(w).Encode(openrtb.BidResponse{ID: bidReq.ID, NoBid: true})
			reqLog.Info("auction complete", "result", "no_bids", "duration_ms", clk.Since(start).Milliseconds())
			am.auctionsTotal.WithLabelValues("no_bids", channel).Inc()
			// Record the no-bid auction too, so the auctions table reflects
			// EVERY ad request (won + unfilled). Without this, fill rate
			// (impressions / auctions) reads ~100% because only won auctions
			// land — the denominator would silently drop every no-bid.
			if pub != nil {
				placementID, publisherID := placementPublisherFromReq(&bidReq)
				floor := 0.0
				if len(bidReq.Imp) > 0 {
					floor = bidReq.Imp[0].BidFloor
				}
				go pub.AuctionComplete(context.WithoutCancel(ctx), events.AuctionCompleteEvent{
					TraceID:     traceID,
					PlacementID: placementID,
					PublisherID: publisherID,
					Channel:     routingChannel,
					NumBids:     0,
					FloorPrice:  floor,
					DurationMs:  clk.Since(start).Milliseconds(),
					Timestamp:   clk.Now(),
				})
			}
			return
		}

		// Deal evaluation: per-bid decision (deal_id, effective floor, preempt).
		// Bids below their per-bid effective floor are dropped. A PG bid short-
		// circuits the auction — that bid wins at the deal price.
		placementID := ""
		publisherID := ""
		if bidReq.Site != nil && bidReq.Site.Publisher != nil && bidReq.Site.Publisher.ID != "" {
			// SSP populates Site.Publisher.ID with the publisher UUID; deal
			// matching uses UUIDs so this is the right key. Fall back to
			// domain when the SSP didn't set it (legacy callers / tests).
			publisherID = bidReq.Site.Publisher.ID
		} else if bidReq.Site != nil {
			publisherID = bidReq.Site.Domain
		}
		if len(bidReq.Imp) > 0 {
			// Imp.TagID carries the placement UUID (set by SSP); fall back
			// to Imp.ID for legacy callers that haven't been updated.
			if bidReq.Imp[0].TagID != "" {
				placementID = bidReq.Imp[0].TagID
			} else {
				placementID = bidReq.Imp[0].ID
			}
		}
		matcher := deals.New(dealCache.All())
		var eligibleBids []auction.Bid
		var preemptBid *auction.Bid
		var preemptPrice float64
		var preemptDealID string
		for i := range bids {
			b := bids[i]
			matches := matcher.Match(deals.Request{
				PublisherID:  publisherID,
				PlacementID:  placementID,
				AdvertiserID: b.AdvertiserID,
				Now:          clk.Now(),
			})
			dec := deals.Decide(b.AdvertiserID, bidReq.Imp[0].BidFloor, matches)
			if dec.Preempt {
				preemptBid = &b
				preemptPrice = dec.EffectiveFloor
				preemptDealID = dec.DealID
				break
			}
			if b.Price < dec.EffectiveFloor {
				reqLog.Debug("bid below effective floor", "bid_dsp", b.DSPID, "price", b.Price, "floor", dec.EffectiveFloor, "deal", dec.DealID)
				am.bidsBelowFloorTotal.WithLabelValues(placementID).Inc()
				continue
			}
			b.DealID = dec.DealID
			eligibleBids = append(eligibleBids, b)
		}

		var (
			winnerBid     auction.Bid
			clearingPrice float64
			winningDealID string
		)
		if preemptBid != nil {
			winnerBid = *preemptBid
			clearingPrice = preemptPrice
			winningDealID = preemptDealID
			reqLog.Info("auction preempted by PG deal", "deal_id", preemptDealID, "winner_dsp", preemptBid.DSPID, "price", preemptPrice)
		} else {
			if len(eligibleBids) == 0 {
				w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
				json.NewEncoder(w).Encode(openrtb.BidResponse{ID: bidReq.ID, NoBid: true})
				reqLog.Info("auction complete", "result", "all_bids_below_floor", "num_bids", len(bids), "duration_ms", clk.Since(start).Milliseconds())
				am.auctionsTotal.WithLabelValues("all_below_floor", channel).Inc()
				return
			}
			auctionReq := auction.AuctionRequest{
				RequestID:  bidReq.ID,
				Channel:    routingChannel,
				PriceMode:  "first_price",
				FloorPrice: bidReq.Imp[0].BidFloor,
				TraceID:    traceID,
			}
			result, err := engine.RunAuction(ctx, eligibleBids, auctionReq)
			if err != nil {
				w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
				json.NewEncoder(w).Encode(openrtb.BidResponse{ID: bidReq.ID, NoBid: true})
				reqLog.Info("auction complete", "result", "no_winner", "error", err.Error(), "duration_ms", clk.Since(start).Milliseconds())
				am.auctionsTotal.WithLabelValues("no_winner", channel).Inc()
				return
			}
			winner := result.Winners[0]
			winnerBid = winner.Bid
			clearingPrice = winner.ClearingPrice
			winningDealID = winner.Bid.DealID
		}

		// Seat = the winning advertiser's UUID (sb.Seat from DSP). Falls back
		// to the DSP node id when seat wasn't set. Without this, the SSP/test
		// harness reads winner.Seat as "dsp-1" instead of the actual
		// advertiser UUID and assertions like "expected seat == advAccID" fail.
		winnerSeat := winnerBid.AdvertiserID
		if winnerSeat == "" {
			winnerSeat = winnerBid.DSPID
		}
		// Carry the winning bid's creative metadata (W/H/Dur/MediaURL/
		// ADomain/BidModel) into the response so the SSP / publisher-
		// adserver doesn't have to re-query the DSP. Critical for the
		// video path: pubad needs the MediaURL + Duration to build the
		// VAST without an extra hop.
		adomain := []string(nil)
		if winnerBid.AdomainHost != "" {
			adomain = []string{winnerBid.AdomainHost}
		}
		resp := openrtb.BidResponse{
			ID:  bidReq.ID,
			Cur: "USD",
			SeatBid: []openrtb.SeatBid{{
				Seat: winnerSeat,
				Bid: []openrtb.BidObj{{
					ID:       fmt.Sprintf("win-%s", traceID),
					ImpID:    bidReq.Imp[0].ID,
					Price:    clearingPrice,
					CID:      winnerBid.CampaignID,
					CrID:     winnerBid.CreativeID,
					W:        winnerBid.Width,
					H:        winnerBid.Height,
					Dur:      winnerBid.Duration,
					MediaURL: winnerBid.MediaURL,
					AdM:      winnerBid.AdM, // native markup — dropped before this fix, breaking native fill
					ADomain:  adomain,
					BidModel: winnerBid.BidModel,
					// Deal ID flows into the response so SSPs/tools see which
					// deal the auction cleared under. The auction handler
					// computed this above (winningDealID); reusing here keeps
					// the response consistent with NATS AuctionWinEvent.DealID.
					DealID: winningDealID,
				}},
			}},
		}

		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		json.NewEncoder(w).Encode(resp)

		reqLog.Info("auction complete",
			"result", "winner",
			"winner_dsp", winnerBid.DSPID,
			"clearing_price", clearingPrice,
			"deal_id", winningDealID,
			"num_bids", len(bids),
			"eligible_bids", len(eligibleBids),
			"duration_ms", clk.Since(start).Milliseconds(),
		)
		// Domain counters for the Pipeline Health dashboard.
		am.auctionsTotal.WithLabelValues("winner", channel).Inc()
		am.clearingPriceUSDTotal.Add(clearingPrice)

		// Tag the auction span with the outcome so dev tools (pub sim, trace
		// explorer) can show winner + clearing price without an extra query.
		// These attributes are dev/ops surface only — the OpenRTB response
		// going back to the SSP/browser deliberately omits them.
		var winnerEndpoint string
		for _, rec := range bidRecords {
			if rec.Bid.DSPID == winnerBid.DSPID {
				winnerEndpoint = rec.Endpoint
				break
			}
		}
		auctionSpan.SetAttributes(
			attribute.String("auction.winner_dsp", winnerBid.DSPID),
			attribute.String("auction.winner_endpoint", winnerEndpoint),
			attribute.Float64("auction.clearing_price", clearingPrice),
			attribute.Int("auction.num_bids", len(bids)),
			attribute.Int("auction.eligible_bids", len(eligibleBids)),
			attribute.String("auction.deal_id", winningDealID),
		)

		// Record the win for the smart router using the winner's endpoint URL
		// (looked up from the bid records). This feeds back into SelectDSPs
		// for future auctions. Also bump the Prometheus wins counter using
		// the same endpoint label so the dashboard can chart bids vs wins
		// per DSP on the same axis.
		for _, rec := range bidRecords {
			if rec.Bid.DSPID == winnerBid.DSPID {
				router.RecordWin(routingChannel, rec.Endpoint)
				am.auctionsWonTotal.WithLabelValues(rec.Endpoint).Inc()
				break
			}
		}

		// Send win/loss notifications asynchronously. Pass WithoutCancel(ctx)
		// so the trace context survives the handler returning (without it the
		// goroutine would lose access to the active span and the notify HTTP
		// calls would appear as detached traces in Jaeger).
		go sendWinLossNotifications(context.WithoutCancel(ctx), client, bidRecords, winnerBid.DSPID, clearingPrice, bidReq.Imp[0].BidFloor, placementID, reqLog)

		// Publish auction events to NATS
		if pub != nil {
			// Detach from the request context: the HTTP handler returns as
			// soon as the response is written, which would cancel ctx and
			// abort the publish mid-flight. WithoutCancel preserves the
			// trace_id value but isolates lifecycle.
			pubCtx := context.WithoutCancel(ctx)
			go func() {
				// AuctionWinEvent - single source of truth for cost
				pub.AuctionWin(pubCtx, events.AuctionWinEvent{
					TraceID:       traceID,
					AuctionID:     traceID,
					WinnerDSP:     winnerBid.DSPID,
					CampaignID:    winnerBid.CampaignID,
					CreativeID:    winnerBid.CreativeID,
					PlacementID:   placementID,
					PublisherID:   publisherID,
					ClearingPrice: clearingPrice,
					Currency:      "USD",
					BidModel:      winnerBid.BidModel,
					Channel:       routingChannel,
					DealID:        winningDealID,
					Timestamp:     clk.Now(),
				})

				// AuctionCompleteEvent - all bids for analytics
				var bidSummaries []events.BidSummary
				for _, b := range bids {
					bidSummaries = append(bidSummaries, events.BidSummary{
						DSPID:      b.DSPID,
						CampaignID: b.CampaignID,
						Price:      b.Price,
						Won:        b.DSPID == winnerBid.DSPID,
					})
				}
				pub.AuctionComplete(pubCtx, events.AuctionCompleteEvent{
					TraceID:       traceID,
					PlacementID:   placementID,
					PublisherID:   publisherID,
					Channel:       routingChannel,
					NumBids:       len(bids),
					WinnerDSP:     winnerBid.DSPID,
					ClearingPrice: clearingPrice,
					FloorPrice:    bidReq.Imp[0].BidFloor,
					DurationMs:    clk.Since(start).Milliseconds(),
					Bids:          bidSummaries,
					Timestamp:     clk.Now(),
				})
			}()
		}
	}
}

// sendWinLossNotifications notifies each DSP whether they won or lost.
// Winner gets price confirmation. Losers get the reason and clearing price
// so they can adjust their bid shading models.
//
// Trace propagation: each outbound HTTP call uses NewRequestWithContext +
// tracing.InjectHTTP so the DSP-side server span becomes a child of this
// fan-out (which is itself a child of the auction). Without InjectHTTP the
// DSP would start a fresh trace_id on the inbound and the win/loss spans
// would appear as detached traces in Jaeger instead of under the auction.
func sendWinLossNotifications(ctx context.Context, client *http.Client, records []dspBidRecord, winnerDSP string, clearingPrice, floorPrice float64, placementID string, log *slog.Logger) {
	ctx, span := tracing.StartSpan(ctx, "exchange.winloss_notify",
		attribute.Int("notify.count", len(records)),
		attribute.String("notify.winner_dsp", winnerDSP),
	)
	defer span.End()

	for _, rec := range records {
		isWin := rec.Bid.DSPID == winnerDSP
		var url string
		if isWin {
			// Win notification — campaign_id is required so the DSP can
			// decrement the right budget counter (Redis IncrBy keyed on
			// campaign_id). Without it, budget caps never trigger.
			url = fmt.Sprintf("%s/v1/openrtb/win?bid_id=%s&price=%.4f&campaign_id=%s&placement_id=%s",
				rec.Endpoint, rec.BidID, clearingPrice, rec.Bid.CampaignID, placementID)
		} else {
			// Loss notification with reason
			reason := 102 // outbid
			if rec.Bid.Price < floorPrice {
				reason = 100 // below floor
			}
			url = fmt.Sprintf("%s/v1/openrtb/loss?bid_id=%s&reason=%d&clearing_price=%.4f&campaign_id=%s&placement_id=%s",
				rec.Endpoint, rec.BidID, reason, clearingPrice, rec.Bid.CampaignID, placementID)
		}
		sendNotify(ctx, client, url, isWin, rec, log)
	}
}

// sendNotify is one win-or-loss notification with its own child span so each
// DSP call shows independently in Jaeger.
func sendNotify(ctx context.Context, client *http.Client, url string, isWin bool, rec dspBidRecord, log *slog.Logger) {
	kind := "loss"
	if isWin {
		kind = "win"
	}
	ctx, span := tracing.StartSpan(ctx, "exchange.notify."+kind,
		attribute.String("notify.dsp", rec.Bid.DSPID),
		attribute.String("notify.endpoint", rec.Endpoint),
	)
	defer span.End()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		span.RecordError(err)
		log.Debug(kind+" notification: build request failed", "dsp", rec.Bid.DSPID, "error", err)
		return
	}
	tracing.InjectHTTP(ctx, req)
	resp, err := client.Do(req)
	if err != nil {
		span.RecordError(err)
		log.Debug(kind+" notification failed", "dsp", rec.Bid.DSPID, "error", err)
		return
	}
	resp.Body.Close()
	span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))
	log.Debug(kind+" notification sent", "dsp", rec.Bid.DSPID)
}

// dspBidRecord tracks which endpoint a bid came from so we can send win/loss notices.
type dspBidRecord struct {
	Bid      auction.Bid
	BidID    string // OpenRTB bid ID
	Endpoint string // DSP's base URL
}

// parseSlowDSPs reads a CSV of DSP indexes (e.g. "0,2") from the inbound
// X-Dev-Slow-DSPs header and returns them as a set for O(1) lookup. Used
// only by the publisher simulator to demonstrate slow-DSP behavior; an
// unauthenticated header on a real bid request would let any caller add
// latency, so this should be gated by debug.endpoints_enabled in callers
// — currently the dev/staging deployment runs with that on and prod will
// not, so prod requests pass empty here and the header is ignored.
// channelForRequest derives the routing channel from the request's imp format
// so the smart router keeps per-format bid stats. Routing on a single blended
// "all" bucket lets display auctions drag a native/audio-only DSP's bid rate
// below the drop threshold, after which that DSP is never called for the format
// it actually bids — and native/audio silently stop filling.
// placementPublisherFromReq derives the placement + publisher IDs from a bid
// request the same way the winning path does (Imp.TagID = placement UUID set
// by the SSP, Site.Publisher.ID = publisher UUID), so a no-bid auction can be
// recorded with the same keys a won one would carry.
func placementPublisherFromReq(req *openrtb.BidRequest) (placementID, publisherID string) {
	if req.Site != nil && req.Site.Publisher != nil && req.Site.Publisher.ID != "" {
		publisherID = req.Site.Publisher.ID
	} else if req.Site != nil {
		publisherID = req.Site.Domain
	}
	if len(req.Imp) > 0 {
		if req.Imp[0].TagID != "" {
			placementID = req.Imp[0].TagID
		} else {
			placementID = req.Imp[0].ID
		}
	}
	return placementID, publisherID
}

func channelForRequest(req *openrtb.BidRequest) string {
	if len(req.Imp) == 0 {
		return constants.ChannelDisplay
	}
	switch {
	case req.Imp[0].Video != nil:
		return "video"
	case req.Imp[0].Audio != nil:
		return "audio"
	case req.Imp[0].Native != nil:
		return "native"
	default:
		return constants.ChannelDisplay
	}
}

func parseSlowDSPs(csv string) map[int]bool {
	if csv == "" {
		return nil
	}
	out := map[int]bool{}
	for _, s := range strings.Split(csv, ",") {
		if i, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			out[i] = true
		}
	}
	return out
}

func fanOutToDSPs(ctx context.Context, client *http.Client, endpoints []string, bidReq openrtb.BidRequest, channel string, slowDSPs map[int]bool, log *slog.Logger, router *optimise.SmartRouter, pub *events.Publisher, traceID string, emit bool) ([]auction.Bid, []dspBidRecord) {
	type dspResult struct {
		dspID    string
		endpoint string
		bids     []auction.Bid
		records  []dspBidRecord
		err      error
		latency  time.Duration
		timedOut bool
		topBid   float64
	}

	ch := make(chan dspResult, len(endpoints))

	// Per-DSP routing telemetry (ADR 0003). Collect one DSPCallEvent per DSP
	// that reports, then publish them fire-and-forget once fan-out finishes —
	// never on the auction's hot path, never affecting the auction outcome. A
	// detached context so the publish survives the handler returning.
	emitEvents := emit && pub != nil
	var callEvents []events.DSPCallEvent
	if emitEvents {
		defer func() {
			if len(callEvents) == 0 {
				return
			}
			evs := callEvents
			pctx := context.WithoutCancel(ctx)
			go func() {
				for i := range evs {
					_ = pub.DSPCall(pctx, evs[i])
				}
			}()
		}()
	}

	for i, endpoint := range endpoints {
		dspID := fmt.Sprintf("dsp-%d", i)
		endpoint := strings.TrimSpace(endpoint)
		i := i // capture for goroutine

		go func() {
			body, _ := json.Marshal(bidReq)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost,
				endpoint+routes.OpenRTBBid, bytes.NewReader(body))
			if err != nil {
				ch <- dspResult{dspID: dspID, endpoint: endpoint, err: err}
				return
			}
			req.Header.Set(constants.HeaderContentType, constants.ContentTypeJSON)
			// Inject the W3C trace context so the DSP-side server span
			// becomes a child of this fanout span in Jaeger.
			tracing.InjectHTTP(ctx, req)
			// Dev-mode: if the inbound auction request flagged this DSP
			// index as "slow," tell the DSP to add artificial latency.
			// DSP gates the header on debug.endpoints_enabled.
			if slowDSPs[i] {
				req.Header.Set("X-Dev-Delay-Ms", "150")
			}

			start := time.Now()
			resp, err := client.Do(req)
			responseTime := time.Since(start)
			if err != nil {
				// Distinguish timeouts from other failures so the router
				// can penalise high-timeout DSPs specifically.
				timedOut := ctx.Err() != nil || strings.Contains(err.Error(), "deadline")
				ch <- dspResult{dspID: dspID, endpoint: endpoint, err: err, latency: responseTime, timedOut: timedOut}
				return
			}
			defer resp.Body.Close()

			respBody, _ := io.ReadAll(resp.Body)
			var bidResp openrtb.BidResponse
			if err := json.Unmarshal(respBody, &bidResp); err != nil {
				ch <- dspResult{dspID: dspID, endpoint: endpoint, err: err, latency: responseTime}
				return
			}

			if bidResp.NoBid || len(bidResp.SeatBid) == 0 {
				ch <- dspResult{dspID: dspID, endpoint: endpoint, latency: responseTime}
				return
			}

			var bids []auction.Bid
			var records []dspBidRecord
			var topBid float64
			for _, sb := range bidResp.SeatBid {
				for _, b := range sb.Bid {
					// Seat is the advertiser/account UUID set by the DSP. We
					// use it for deal allowlist matching; ADomain is the creative
					// landing domain and used elsewhere for safety/blocklists.
					adomain := ""
					if len(b.ADomain) > 0 {
						adomain = b.ADomain[0]
					}
					bm := b.BidModel
					if bm == "" {
						bm = "cpm"
					}
					bid := auction.Bid{
						DSPID:        dspID,
						CampaignID:   b.CID,
						CreativeID:   b.CrID,
						Price:        b.Price,
						Currency:     bidResp.Cur,
						BidModel:     bm,
						Duration:     b.Dur,
						Width:        b.W,
						Height:       b.H,
						MediaURL:     b.MediaURL,
						AdM:          b.AdM,
						AdvertiserID: sb.Seat,
						AdomainHost:  adomain,
						ResponseTime: responseTime,
					}
					bids = append(bids, bid)
					records = append(records, dspBidRecord{
						Bid:      bid,
						BidID:    b.ID,
						Endpoint: endpoint,
					})
					if b.Price > topBid {
						topBid = b.Price
					}
				}
			}
			ch <- dspResult{dspID: dspID, endpoint: endpoint, bids: bids, records: records, latency: responseTime, topBid: topBid}
		}()
	}

	var allBids []auction.Bid
	var allRecords []dspBidRecord
	received := 0
	// Early-finish: once the fan-out context deadline elapses, stop waiting
	// for in-flight DSP goroutines. They'll be cancelled via ctx propagation
	// and exit on their own (their channel sends become wasted writes into a
	// buffered channel that nobody reads, which is fine — the channel has
	// capacity = len(endpoints)). Without this, a single slow DSP whose
	// HTTP call hasn't yet returned (despite ctx cancel) would force the
	// auction to wait for it just to read its "timed out" result.
	for received < len(endpoints) {
		select {
		case result := <-ch:
			// Endpoint URL is the stable key for routing stats (DSP indexes
			// can shift if config changes, URLs don't).
			bidReceived := len(result.bids) > 0
			router.RecordCall(channel, result.endpoint, bidReceived, result.topBid, result.latency, result.timedOut)
			if emitEvents {
				callEvents = append(callEvents, events.DSPCallEvent{
					TraceID: traceID, AuctionID: traceID, Channel: channel,
					DSPEndpoint: result.endpoint, BidReceived: bidReceived,
					BidPriceUSD: result.topBid, LatencyMs: result.latency.Milliseconds(),
					TimedOut: result.timedOut, Timestamp: time.Now(),
				})
			}
			if result.err != nil {
				log.Warn("dsp call failed", "dsp", result.dspID, "endpoint", result.endpoint, "error", result.err)
			} else {
				allBids = append(allBids, result.bids...)
				allRecords = append(allRecords, result.records...)
			}
			received++
		case <-ctx.Done():
			log.Debug("fan-out deadline elapsed before all DSPs reported",
				"received", received, "total", len(endpoints))
			return allBids, allRecords
		}
	}
	return allBids, allRecords
}

// rebuild trigger
