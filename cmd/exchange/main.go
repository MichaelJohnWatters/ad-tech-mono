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
	"sync/atomic"
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
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/grpcx"
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
	// Assigned below once the DB is available; the closure captures it by
	// reference (auctions only run after setup completes).
	var partnerEP *partnerEndpoints
	dspEndpointsFn := func() []string {
		raw := keys.Exchange.DSPEndpoints.Get(cfg)
		parts := strings.Split(raw, ",")
		// Merge 'active' DSP partners from the registry warm cache when enabled —
		// same "endpoint[;seat=][;notify=]" format, so the loop below handles them.
		if partnerEP != nil && keys.Exchange.PartnerRegistryEnabled.Get(cfg) {
			parts = append(parts, partnerEP.Snapshot()...)
		}
		out := make([]string, 0, len(parts))
		seen := make(map[string]struct{}, len(parts))
		notify := make(map[string]string, len(parts))
		seats := make(map[string]string, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			endpoint, notifyBase, seat := splitDSPEndpoint(p)
			// Dedup: the same DSP in both config and the registry must be called
			// ONCE (else two parallel calls + two bids from one seat). Config comes
			// first in `parts`, so its seat/notify win.
			if _, dup := seen[endpoint]; dup {
				continue
			}
			seen[endpoint] = struct{}{}
			out = append(out, endpoint)
			if notifyBase != "" {
				notify[endpoint] = notifyBase
			}
			if seat != "" {
				seats[endpoint] = seat
			}
		}
		// The clean bid endpoint is the stable key everywhere (router stats,
		// dsp_calls, warm-start); the notify base rides out-of-band so
		// win/loss URLs don't inherit a grpc:// scheme they can't use.
		dspNotifyBases.Store(notify)
		// The trusted billable seat per external partner (data-fee attribution):
		// operator-controlled, so a bidder can't self-declare its way out of, or
		// onto, a receivable.
		dspSeats.Store(seats)
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
		natsBus.EnsureStreamWithRetry(ctx, events.StreamName, []string{events.StreamSubjects})
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

	// Event spool: a failed NATS publish (deadline/stall/restart) is absorbed
	// to disk and replayed on reconnect instead of silently dropped — the
	// 2026-08-05 VM seizure cost 146k events (auction wins + completes) to
	// exactly that. Pressure feeds X-Event-Pressure on auction responses so
	// the SSP's front-door throttle can shed before the spool caps out.
	if pub != nil {
		if sp, err := events.NewSpool(events.SpoolDirFromEnv(), events.DefaultSpoolCap, metrics.Registry()); err != nil {
			log.Error("event spool init failed — publishes remain at-most-once", "error", err)
		} else {
			stopSpool := pub.EnableSpool(context.Background(), sp)
			lc.OnShutdown("event-spool", func(context.Context) error { stopSpool(); return nil })
			log.Info("event spool armed", "dir", events.SpoolDirFromEnv())
		}
	}

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
	// Live routing knobs: every threshold the router acts on reads config
	// per selection, so the staff portal's exchange.routing_* edits apply
	// within the config poll interval — no restart.
	router.SetKnobs(func() optimise.Knobs {
		k := optimise.DefaultKnobs()
		k.Enabled = keys.Exchange.RoutingEnabled.Get(cfg)
		k.MinCalls = int64(keys.Exchange.RoutingMinCalls.Get(cfg))
		k.MinBidRate = keys.Exchange.RoutingMinBidRate.Get(cfg)
		k.MaxTimeoutRate = keys.Exchange.RoutingMaxTimeoutRate.Get(cfg)
		k.ExplorePct = keys.Exchange.RoutingExplorePct.Get(cfg)
		k.LatencySoft = keys.Exchange.RoutingLatencySoft.Get(cfg)
		k.LatencyHard = keys.Exchange.RoutingLatencyHard.Get(cfg)
		k.RecencyWindow = int64(keys.Exchange.RoutingRecencyWindow.Get(cfg))
		if raw := keys.Exchange.RoutingNeverSkip.Get(cfg); raw != "" {
			k.NeverSkip = parseNeverSkip(raw)
		}
		return k
	})
	// Warm-start routing from reporting's dsp_calls history (ADR 0003) so a
	// restarted exchange isn't cold. Async + fail-open — never blocks boot.
	go warmStartRouter(cfg, router, log)
	// Cross-replica router sync: periodic reseed from the cluster-global
	// dsp_calls aggregate + reset broadcast (see routingsync.go). Own bus
	// handle, same lifecycle independence as the warm-cache invalidate bus.
	routerSync := startRoutingSync(cfg, router, connectInvalidateBus(cfg, log), log)

	mux := http.NewServeMux()
	// On-demand profiler (internal mux only; zero cost until a profile is
	// pulled). Block/mutex profiling stays off until armed — see
	// middleware.SetProfileRates.
	middleware.AttachPprof(mux)
	mux.Handle(routes.Healthz, hlth.LivenessHandler())
	mux.Handle(routes.Readyz, hlth.ReadinessHandler())
	mux.Handle(routes.Metrics, metrics.Handler())

	// Warm cache of active DSP partners (#112) — merged into the auction fan-out
	// when exchange.partner_registry_enabled is on. Background-refreshed so the
	// hot path reads an in-process snapshot, never a query.
	partnerCtx, partnerCancel := context.WithCancel(context.Background())
	lc.OnShutdown("partner-endpoints", func(context.Context) error { partnerCancel(); return nil })
	partnerEP = startPartnerEndpoints(partnerCtx, cfg, connectInvalidateBus(cfg, log), log)

	// Debug surface — all behind debug.endpoints_enabled (default true in
	// dev, expected false in prod overlays). Reads + mutations both gated
	// so the prod surface is purely the auction/win/loss/Prebid paths.
	if keys.Debug.EndpointsEnabled.Get(cfg) {
		refreshables := []warm.Refreshable{dealCache}
		if adsTxtWarm != nil {
			refreshables = append(refreshables, adsTxtWarm)
		}
		if partnerEP != nil {
			refreshables = append(refreshables, partnerEP)
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
				// Broadcast so ALL replicas reset — a pod-local reset would
				// leave N-1 pods trained on history the caller meant to wipe.
				routerSync.broadcastReset(r.Context())
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
	retailMinRelFn := func() float64 { return keys.Exchange.RetailMinRelevance.Get(cfg) }
	auction := auctionHandler(log, clk, engine, httpClient, knobs.BidTimeout.Value, dspEndpointsFn, knobs.Channel, debugEnabledFn, pub, adsTxtCache, adsTxtGate, schainGate, signReq, dealCache, router, auctionM, emitDSPCallFn, retailMinRelFn)
	// Partner inbound auth (#112): authenticate the EXTERNAL HTTP OpenRTB surfaces
	// against a per-partner sandbox key. Wrapped at the OUTERMOST layer of each
	// external handler — including Prebid, so a rejected caller is 401'd BEFORE the
	// Prebid handler reads the body / publishes identity signals. The internal gRPC
	// twin (our own SSP) is handed the UNGATED `auction` below (trusted transport,
	// no partner key). Reuses the shared middleware.PartnerInboundAuth.
	partnerAuth := middleware.PartnerInboundAuth(adcertSecrets, func() bool { return keys.Exchange.InboundPartnerAuthStrict.Get(cfg) }, log)
	mux.Handle(routes.OpenRTBAuction, partnerAuth(http.HandlerFunc(auction)))
	mux.HandleFunc(routes.AdCertKey, adCertKeyHandler(adcertSigner))
	mux.HandleFunc(routes.OpenRTBWin, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc(routes.OpenRTBLoss, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })

	// Prebid Server-compatible bidder endpoint. See pkg/prebid + docs/PLAN.md
	// → "Prebid Server Integration". Reuses the same auction path with the inbound
	// floor policy + opaque deal-id logging applied first; gated at the outer layer.
	mux.Handle(routes.PrebidAuction, partnerAuth(prebidAuctionHandler(cfg, auction, idPub, log)))
	mux.HandleFunc(routes.PrebidSetUID, prebidSetUIDHandler(log))

	handler := tracing.HTTPMiddleware(constants.ServiceExchange)(metrics.Wrap(middleware.CORS(mux)))

	// Internal gRPC twin of the auction endpoint — our SSP's fast path. Handed the
	// UNGATED auction: internal callers are trusted and carry no partner key.
	startInternalGRPC(lc, cfg, log, metrics, auction)

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

func auctionHandler(log *slog.Logger, clk clock.Clock, engine *auction.Engine, client *http.Client, bidTimeoutFn func() time.Duration, dspEndpointsFn func() []string, channelFn func() string, debugEnabledFn func() bool, pub *events.Publisher, adsTxt *fraud.AdsTxtCache, adsTxtGate func(string) (bool, string), schainGate func(*openrtb.BidRequest, string) (bool, string), signReq func(*openrtb.BidRequest), dealCache *warm.Cache[models.Deal], router *optimise.SmartRouter, am *auctionMetrics, emitDSPCallFn func(traceID string) bool, retailMinRelFn func() float64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Event-layer backpressure signal: the SSP's front-door throttle sheds
		// incoming serve requests as this rises (each serve spawns ~5 events),
		// letting a stressed spool drain instead of growing to its cap.
		if pub != nil {
			if pr := pub.Pressure(); pr > 0 {
				w.Header().Set("X-Event-Pressure", strconv.Itoa(pr))
			}
		}

		// Phase clock: obsPhase records the time since the previous boundary
		// under the named phase and restarts the clock. Early returns simply
		// record fewer phases — the histogram is per-phase, not per-request,
		// so partial coverage is fine.
		phaseStart := time.Now()
		obsPhase := func(name string) {
			am.phaseDuration.WithLabelValues(name).Observe(time.Since(phaseStart).Seconds())
			phaseStart = time.Now()
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
				json.NewEncoder(w).Encode(openrtb.BidResponse{ID: bidReq.ID, NoBid: true, NBR: openrtb.NBRAdsTxtUnauthorised, NBRReason: reason})
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
			json.NewEncoder(w).Encode(openrtb.BidResponse{ID: bidReq.ID, NoBid: true, NBR: openrtb.NBRSChainInvalid, NBRReason: reason})
			reqLog.Info("auction rejected", "reason", reason)
			am.auctionsTotal.WithLabelValues("rejected_schain", channel).Inc()
			return
		}

		// ads.cert: sign the request (after any schain append) so DSPs can
		// verify it authentically came from this exchange. No-op when signing
		// is unconfigured.
		signReq(&bidReq)
		obsPhase("gates")

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
		// or timeout patterns drop a DSP for this auction — except on the
		// deterministic ε-probe slice (exchange.routing_explore_pct), which
		// keeps a skipped DSP's stats flowing so it can earn its way back.
		dspEndpoints := dspEndpointsFn()
		selectedEndpoints := router.SelectDSPsForTrace(routingChannel, dspEndpoints, traceID)
		if len(selectedEndpoints) == 0 {
			// Safety floor: if the router would skip everyone (cold start edge
			// case or learned-bad state), fall back to the full list. We never
			// want to silently no-bid because of routing.
			selectedEndpoints = dspEndpoints
		}

		obsPhase("routing")
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

		bids, bidRecords := fanOutToDSPs(fanCtx, client, selectedEndpoints, bidReq, routingChannel, slowDSPs, reqLog, router, pub, traceID, emitDSPCallFn(traceID), am)
		fanSpan.SetAttributes(attribute.Int("bids.received", len(bids)))
		fanSpan.End()
		obsPhase("fanout")

		if len(bids) == 0 {
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			noBidResp := openrtb.BidResponse{ID: bidReq.ID, NoBid: true}
			if pub != nil {
				noBidResp.EventPressure = pub.Pressure()
			}
			json.NewEncoder(w).Encode(noBidResp)
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

		// multiWinners holds every winner when the strategy fills more than one
		// slot in a single auction (in-game scene surfaces, retail sponsored
		// slots). The response exposes them all; the win event / billing still
		// fire for position 1 (multi-winner billing is a documented follow-up).
		var multiWinners []auction.Winner
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
			if routingChannel == constants.ChannelRetail {
				// The shopper's browsed categories (Site.Cat) feed the relevance-
				// weighted ranking across the DSP's product slate; SlotCount is the
				// number of sponsored slots on the results page (imp.ext.surfaces),
				// each returned as a ranked multi-winner and billed on its own
				// sub-trace.
				auctionReq.SlotCount = 1
				if bidReq.Imp[0].Ext != nil && bidReq.Imp[0].Ext.Surfaces > 0 {
					auctionReq.SlotCount = bidReq.Imp[0].Ext.Surfaces
				}
				if bidReq.Site != nil {
					auctionReq.RetailCategories = bidReq.Site.Cat
				}
				auctionReq.MinRelevance = retailMinRelFn()
			}
			if routingChannel == constants.ChannelInGame {
				// An intrinsic in-game placement is a whole scene of surfaces filled
				// in one auction (Format "intrinsic" → batch strategy). Surfaces is
				// the number of billboards; the batch strategy applies one-advertiser
				// and one-category-per-scene competitive separation across them.
				if bidReq.Imp[0].Ext != nil {
					auctionReq.Format = bidReq.Imp[0].Ext.PlacementType
					auctionReq.SlotCount = bidReq.Imp[0].Ext.Surfaces
				}
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
			if result.IsMultiWinner && len(result.Winners) > 1 {
				multiWinners = result.Winners
			}
		}
		obsPhase("dealeval")

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
			// Trusted billable seat (data-fee attribution) — from which configured
			// endpoint won, not winnerSeat (which is the self-declared response seat).
			// Empty for internal winners, so the SSP skips them.
			SettlementSeat: winnerBid.SettlementSeat,
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
		// Multi-winner strategies (in-game scene surfaces, retail slate) expose
		// EVERY winner in the response — one SeatBid per winning advertiser, in
		// rank/position order — so the caller sees the whole filled scene. Position
		// 1 stays first (its SeatBid built above); the rest are appended. Win event /
		// billing above still bind to position 1 (multi-winner billing is a follow-up).
		if len(multiWinners) > 1 {
			seatBids := make([]openrtb.SeatBid, 0, len(multiWinners))
			for _, wr := range multiWinners {
				wb := wr.Bid
				seat := wb.AdvertiserID
				if seat == "" {
					seat = wb.DSPID
				}
				var wadomain []string
				if wb.AdomainHost != "" {
					wadomain = []string{wb.AdomainHost}
				}
				seatBids = append(seatBids, openrtb.SeatBid{
					Seat: seat,
					Bid: []openrtb.BidObj{{
						// ID is the per-surface impression id (sub-trace): the
						// renderer fires this surface's impression with tid=ID so it
						// bills independently of the other surfaces.
						ID:       surfaceTrace(traceID, wr.Position),
						ImpID:    bidReq.Imp[0].ID,
						Price:    wr.ClearingPrice,
						CID:      wb.CampaignID,
						CrID:     wb.CreativeID,
						W:        wb.Width,
						H:        wb.Height,
						MediaURL: wb.MediaURL,
						AdM:      wb.AdM,
						ADomain:  wadomain,
						BidModel: wb.BidModel,
						DealID:   wb.DealID,
					}},
				})
			}
			resp.SeatBid = seatBids
		}

		if pub != nil {
			resp.EventPressure = pub.Pressure()
		}
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		json.NewEncoder(w).Encode(resp)
		obsPhase("finalize")

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
				// AuctionWinEvent - single source of truth for cost. Multi-winner
				// auctions (in-game surfaces, retail slots) emit ONE per winner,
				// each on its own sub-trace so every surface bills independently;
				// single-winner auctions emit exactly one on the main trace (all
				// other channels unchanged).
				type winRec struct {
					bid   auction.Bid
					price float64
					trace string
				}
				var wins []winRec
				if len(multiWinners) > 1 {
					for _, wr := range multiWinners {
						wins = append(wins, winRec{bid: wr.Bid, price: wr.ClearingPrice, trace: surfaceTrace(traceID, wr.Position)})
					}
				} else {
					wins = []winRec{{bid: winnerBid, price: clearingPrice, trace: traceID}}
				}
				// Public audience segments the SSP stamped on this request
				// (consent-gated there). Request-level context, so every
				// winner of a multi-winner auction carries the same list.
				var reqSegments []string
				if bidReq.User != nil && bidReq.User.Ext != nil {
					reqSegments = bidReq.User.Ext.Segments
				}
				for _, wn := range wins {
					pub.AuctionWin(pubCtx, events.AuctionWinEvent{
						TraceID:       wn.trace,
						AuctionID:     traceID,
						WinnerDSP:     wn.bid.DSPID,
						CampaignID:    wn.bid.CampaignID,
						CreativeID:    wn.bid.CreativeID,
						PlacementID:   placementID,
						PublisherID:   publisherID,
						ClearingPrice: wn.price,
						Currency:      "USD",
						BidModel:      wn.bid.BidModel,
						Channel:       routingChannel,
						DealID:        wn.bid.DealID,
						Segments:      reqSegments,
						Timestamp:     clk.Now(),
					})
				}

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

// dspNotifyBases maps a DSP's bid endpoint to the HTTP base URL its OpenRTB
// win/loss notices are sent to. Win/loss notices are HTTP by design (they're
// the OpenRTB nurl/lurl mechanism), so a DSP whose BID edge rides the
// internal gRPC twin declares where notices go with a ";notify=<http-base>"
// suffix on its exchange.dsp_endpoints entry, e.g.
//
//	grpc://dsp-internal-grpc:8182;notify=http://dsp-internal:8082
//
// Plain http:// entries need no suffix — their bid endpoint doubles as the
// notify base. Refreshed by dspEndpointsFn on every auction (live config).
var dspNotifyBases atomic.Value // map[string]string

// dspSeats maps a bid endpoint → the operator-configured trusted billable seat
// (from a ";seat=" suffix on its exchange.dsp_endpoints entry). Refreshed by
// dspEndpointsFn on every auction (live config). This is the identity a
// data-fee receivable is attributed to — bound to WHICH configured endpoint won,
// never to the seat a bidder self-declares in its response body.
var dspSeats atomic.Value // map[string]string

// splitDSPEndpoint separates one exchange.dsp_endpoints entry into the bid
// endpoint and its optional ";"-separated params. Two params are recognised,
// order-independent, and may coexist:
//
//	;notify=<http-base>  — where win/loss notices go (grpc:// bid edges need it)
//	;seat=<id>           — the operator's canonical BILLABLE seat for this
//	                       partner: the trusted identity a data-fee receivable is
//	                       attributed to, so a bidder can't self-declare a seat to
//	                       dodge or misdirect what it owes (see docs/datafee-seat-integrity-plan.md)
//
// e.g. http://partner:9100/bid;seat=acme-dsp;notify=http://partner:9100
// Unknown params are ignored (forward-compatible).
func splitDSPEndpoint(entry string) (endpoint, notifyBase, seat string) {
	parts := strings.Split(entry, ";")
	endpoint = strings.TrimSpace(parts[0])
	for _, p := range parts[1:] {
		p = strings.TrimSpace(p)
		if v, ok := strings.CutPrefix(p, "notify="); ok {
			notifyBase = strings.TrimSpace(v)
		} else if v, ok := strings.CutPrefix(p, "seat="); ok {
			seat = strings.TrimSpace(v)
		}
	}
	return endpoint, notifyBase, seat
}

// parseNeverSkip builds the never-skip endpoint set from the CSV config
// value. Entries run through splitDSPEndpoint so an operator can paste a
// full exchange.dsp_endpoints entry — ";notify=" suffix and all — and it
// still matches the CLEAN bid endpoint the router keys stats by. Without
// the normalisation a suffixed entry silently never matched.
func parseNeverSkip(raw string) map[string]struct{} {
	set := make(map[string]struct{})
	for _, e := range strings.Split(raw, ",") {
		if e = strings.TrimSpace(e); e != "" {
			endpoint, _, _ := splitDSPEndpoint(e)
			set[endpoint] = struct{}{}
		}
	}
	return set
}

// notifyBaseFor resolves where a DSP's win/loss notices go: the declared
// ;notify= base if any, else the bid endpoint itself.
func notifyBaseFor(endpoint string) string {
	if m, ok := dspNotifyBases.Load().(map[string]string); ok {
		if v := m[endpoint]; v != "" {
			return v
		}
	}
	return endpoint
}

// trustedSeatFor returns the billable seat for a bid that came from `endpoint`,
// for data-fee attribution. INTERNAL demand (grpc:// — our own DSP) returns ""
// (data fees are out of scope for demand we own, and its self-declared advertiser
// UUID is already trustworthy). For an EXTERNAL (http://) partner it returns the
// operator-configured ";seat=" id, falling back to the endpoint URL — both
// un-forgeable, unlike the seat a bidder writes in its response. Empty return =
// "not a billable external seat", which the SSP uses to skip attribution.
func trustedSeatFor(endpoint string) string {
	if grpcx.IsURL(endpoint) {
		return "" // internal, our own demand
	}
	if m, ok := dspSeats.Load().(map[string]string); ok {
		if v := m[endpoint]; v != "" {
			return v
		}
	}
	return endpoint // un-forgeable fallback until a ;seat= is configured
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

	// minToWin is the TRUE market clearing for the winner: the highest competing
	// (losing) bid — what the winner actually had to beat — floored at the auction
	// floor. This is the honest price signal for the DSP's bid-shading curve. The
	// winner's own paid bid (`clearingPrice`, first-price) just echoes what they
	// bid and tells the curve nothing about the market, so we send BOTH: `price`
	// (what they pay, for budget) and `clear_price` (minToWin, for the curve).
	minToWin := floorPrice
	for _, rec := range records {
		if rec.Bid.DSPID != winnerDSP && rec.Bid.Price > minToWin {
			minToWin = rec.Bid.Price
		}
	}

	for _, rec := range records {
		isWin := rec.Bid.DSPID == winnerDSP
		// Notices are OpenRTB HTTP even when the bid edge rode the internal
		// gRPC twin — resolve the HTTP notify base for this endpoint. A
		// grpc:// base here means the ;notify= suffix is missing from the
		// exchange.dsp_endpoints entry: undeliverable, and silently dropped
		// win notices break budget caps — say so loudly.
		base := notifyBaseFor(rec.Endpoint)
		if grpcx.IsURL(base) {
			log.Error("win/loss notify base is grpc:// — add ';notify=<http-base>' to the exchange.dsp_endpoints entry",
				"endpoint", rec.Endpoint, "dsp", rec.Bid.DSPID)
			continue
		}
		var url string
		if isWin {
			// Win notification — campaign_id is required so the DSP can
			// decrement the right budget counter (Redis IncrBy keyed on
			// campaign_id). Without it, budget caps never trigger.
			url = fmt.Sprintf("%s/v1/openrtb/win?bid_id=%s&price=%.4f&clear_price=%.4f&campaign_id=%s&placement_id=%s",
				base, rec.BidID, clearingPrice, minToWin, rec.Bid.CampaignID, placementID)
		} else {
			// Loss notification with reason
			reason := 102 // outbid
			if rec.Bid.Price < floorPrice {
				reason = 100 // below floor
			}
			url = fmt.Sprintf("%s/v1/openrtb/loss?bid_id=%s&reason=%d&clearing_price=%.4f&campaign_id=%s&placement_id=%s",
				base, rec.BidID, reason, clearingPrice, rec.Bid.CampaignID, placementID)
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

// surfaceTrace derives a distinct per-surface impression id for a multi-winner
// auction (in-game scene surfaces, retail sponsored slots) so each surface's
// impression bills INDEPENDENTLY — billing dedups on trace_id, so distinct
// sub-traces = distinct billed impressions. Single-winner auctions keep the main
// trace unchanged (no suffix), so the exactly-once path for every other channel
// is untouched. The `::s{n}` suffix stays greppable back to the parent auction.
func surfaceTrace(mainTrace string, position int) string {
	return fmt.Sprintf("%s::s%d", mainTrace, position)
}

// firstOrEmpty returns the first element of a string slice, or "" when empty.
// Used to collapse OpenRTB BidObj.Cat ([]string) into the single primary product
// category the retail relevance scorer compares against.
func firstOrEmpty(s []string) string {
	if len(s) > 0 {
		return s[0]
	}
	return ""
}

func channelForRequest(req *openrtb.BidRequest) string {
	if len(req.Imp) == 0 {
		return constants.ChannelDisplay
	}
	imp := req.Imp[0]
	// The emerging channels (DOOH / retail / in-game) have no distinct OpenRTB
	// media object — a DOOH screen carries a banner-shaped creative — so they ride
	// imp.ext.channel. Honour it before falling back to media-type detection.
	if imp.Ext != nil {
		switch imp.Ext.Channel {
		case constants.ChannelDOOH, constants.ChannelRetail, constants.ChannelInGame:
			return imp.Ext.Channel
		}
	}
	switch {
	case imp.Video != nil:
		return "video"
	case imp.Audio != nil:
		return "audio"
	case imp.Native != nil:
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

func fanOutToDSPs(ctx context.Context, client *http.Client, endpoints []string, bidReq openrtb.BidRequest, channel string, slowDSPs map[int]bool, log *slog.Logger, router *optimise.SmartRouter, pub *events.Publisher, traceID string, emit bool, am *auctionMetrics) ([]auction.Bid, []dspBidRecord) {
	type dspResult struct {
		dspID       string
		endpoint    string
		bids        []auction.Bid
		records     []dspBidRecord
		err         error
		latency     time.Duration
		timedOut    bool
		topBid      float64
		noBidReason string // the DSP's stated reason when it declined (e.g. adcert_invalid)
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
			var respBody []byte
			var responseTime time.Duration
			if grpcx.IsURL(endpoint) {
				// Our own DSP: ride the gRPC twin of /v1/openrtb/bid. Only
				// demand we own is ever configured with a grpc:// endpoint —
				// third-party DSPs always take the OpenRTB HTTP branch below.
				// Same JSON, same bid handler on the far side; the client
				// *http.Client timeout is mirrored as a ctx deadline.
				var hdrs map[string]string
				if slowDSPs[i] {
					hdrs = map[string]string{"X-Dev-Delay-Ms": "150"}
				}
				cctx := ctx
				if client.Timeout > 0 {
					var cancel context.CancelFunc
					cctx, cancel = context.WithTimeout(ctx, client.Timeout)
					defer cancel()
				}
				start := time.Now()
				_, rb, err := grpcx.Bid(cctx, grpcx.Target(endpoint), body, hdrs)
				responseTime = time.Since(start)
				if err != nil {
					timedOut := cctx.Err() != nil || strings.Contains(err.Error(), "deadline")
					ch <- dspResult{dspID: dspID, endpoint: endpoint, err: err, latency: responseTime, timedOut: timedOut}
					return
				}
				respBody = rb
			} else {
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
				responseTime = time.Since(start)
				if err != nil {
					// Distinguish timeouts from other failures so the router
					// can penalise high-timeout DSPs specifically.
					timedOut := ctx.Err() != nil || strings.Contains(err.Error(), "deadline")
					ch <- dspResult{dspID: dspID, endpoint: endpoint, err: err, latency: responseTime, timedOut: timedOut}
					return
				}
				defer resp.Body.Close()
				respBody, _ = io.ReadAll(resp.Body)
			}
			var bidResp openrtb.BidResponse
			if err := json.Unmarshal(respBody, &bidResp); err != nil {
				ch <- dspResult{dspID: dspID, endpoint: endpoint, err: err, latency: responseTime}
				return
			}

			if bidResp.NoBid || len(bidResp.SeatBid) == 0 {
				// Preserve the DSP's stated no-bid reason (e.g. an ads.cert
				// block) so it lands in dsp_calls + the trace, instead of being
				// flattened into the exchange's aggregated no-bid.
				ch <- dspResult{dspID: dspID, endpoint: endpoint, latency: responseTime, noBidReason: bidResp.NBRReason}
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
						AdvertiserID: sb.Seat, // self-declared — display/deal-match only, NOT billing
						// Trusted billable seat = which configured endpoint this bid
						// came from (data-fee attribution can't ride the self-declared seat).
						SettlementSeat: trustedSeatFor(endpoint),
						AdomainHost:    adomain,
						// Product category (OpenRTB BidObj.Cat) — the retail relevance
						// signal scored against the shopper's browsed categories.
						Category:     firstOrEmpty(b.Cat),
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
	fanoutStart := time.Now()
	reported := make(map[string]bool, len(endpoints))
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
			reported[result.endpoint] = true
			bidReceived := len(result.bids) > 0
			router.RecordCall(channel, result.endpoint, bidReceived, result.topBid, result.latency, result.timedOut)
			// Prometheus mirror of the router's per-DSP view. This counter was
			// registered (and charted by "Bids / Wins / No-bids by DSP") but
			// never incremented — the panel showed wins only. "error" is a
			// third decision the panel can add: transport failure/deadline,
			// distinct from an explicit no-bid.
			if am != nil {
				decision := "no_bid"
				switch {
				case result.err != nil:
					decision = "error"
				case bidReceived:
					decision = "bid"
				}
				am.bidsReceivedTotal.WithLabelValues(result.endpoint, decision).Inc()
			}
			if emitEvents {
				callEvents = append(callEvents, events.DSPCallEvent{
					TraceID: traceID, AuctionID: traceID, Channel: channel,
					DSPEndpoint: result.endpoint, BidReceived: bidReceived,
					BidPriceUSD: result.topBid, LatencyMs: result.latency.Milliseconds(),
					TimedOut: result.timedOut, NoBidReason: result.noBidReason, Timestamp: time.Now(),
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
			// Record a timeout for every leg still in flight. Without this
			// the router was BLIND to the worst offenders: a DSP slower than
			// the whole fan-out deadline never got a RecordCall, so its
			// timeout rate read 0 and MaxTimeoutRate could never skip it —
			// every auction kept paying the full deadline for a DSP that
			// never answers in time. (Caught by the slowpoke scenario DSP on
			// its first run: 500ms±25% delay, timeout_rate stuck at 0.)
			elapsed := time.Since(fanoutStart)
			for _, ep := range endpoints {
				ep = strings.TrimSpace(ep)
				if reported[ep] {
					continue
				}
				// Cold-start grace lives in RecordFanoutDeadline: a leg
				// inside its MinCalls warm-up records nothing (locally OR as
				// telemetry — the CH rows feed the reseed, so an emitted
				// event would re-poison what the grace just protected).
				if !router.RecordFanoutDeadline(channel, ep, elapsed) {
					continue
				}
				if am != nil {
					am.bidsReceivedTotal.WithLabelValues(ep, "timeout").Inc()
				}
				if emitEvents {
					callEvents = append(callEvents, events.DSPCallEvent{
						TraceID: traceID, AuctionID: traceID, Channel: channel,
						DSPEndpoint: ep, BidReceived: false,
						LatencyMs: elapsed.Milliseconds(), TimedOut: true,
						NoBidReason: "fanout_deadline", Timestamp: time.Now(),
					})
				}
			}
			return allBids, allRecords
		}
	}
	return allBids, allRecords
}

// rebuild trigger
