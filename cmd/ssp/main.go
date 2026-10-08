// cmd/ssp is the Supply-Side Platform service.
// Manages publisher inventory, generates bid requests.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
	audstore "github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store"
	audcached "github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store/cached"
	audiencepg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store/postgres"
	audpreload "github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store/preload"
	audprobe "github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store/probe"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache"
	cacheredis "github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache/redis"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache/warm"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events/natsbus"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/floors"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/grpcx"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/health"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/identity"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/idgen"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/native"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/secrets"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
	_ "github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
)

func main() {
	clk := clock.Real{}
	log := logger.New(constants.ServiceSSP)
	sc := config.Setup(constants.ServiceSSP, keys.SSPSchema(), log)
	cfg := sc.Cfg
	_ = sc
	hlth := health.New()
	lc := lifecycle.New(log)

	port := keys.SSP.Port.Get(cfg)
	exchangeURL := keys.SSP.ExchangeURL.Get(cfg)
	// This platform's advertising-system domain, used as the asi of the first
	// schain node on outbound bid requests. Read once at boot (static tier).
	sellerDomain := keys.SSP.SellerDomain.Get(cfg)
	// Data-residency home region of this deployment (PLAN #111). A request whose
	// regs.ext.data_residency names a different region has its user-level data
	// suppressed (see requestStoresUserData). Deployment-static; read once.
	sspHomeRegion = keys.Platform.Region.Get(cfg)

	// OTel — required so HTTPMiddleware's server span has a real trace ID
	// that flows into logs / NATS events / analytics store.
	otelShutdown := tracing.Init(context.Background(), tracing.Config{
		ServiceName:    constants.ServiceSSP,
		ServiceVersion: keys.Otel.ServiceVersion.Get(cfg),
		Endpoint:       keys.Otel.Endpoint.Get(cfg),
		SampleRatio:    keys.Otel.SampleRatio.Get(cfg),
		Log:            log,
	})
	lc.OnShutdown("otel", func(ctx context.Context) error { return otelShutdown(ctx) })

	placementCache := startPlacementCache(cfg, clk, log)
	if placementCache != nil {
		lc.OnShutdown("placement-cache", func(_ context.Context) error { placementCache.Stop(); return nil })
	}

	// Audience segment store: SSP looks up user→segments on each bid request
	// and stamps user.ext.segments on the outbound OpenRTB. Nil-tolerant —
	// if Postgres is unreachable the SSP keeps serving without segments.
	l2 := connectRedis(cfg, log)
	audienceStore, audiencePreloader, audiencePG, audienceStop := openAudienceStore(cfg, l2, log)
	lc.OnShutdown("audience-store", func(_ context.Context) error { audienceStop(); return nil })

	// Warm map of public segment → IAB Audience Taxonomy id, used to stamp
	// standards-interoperable user.data on outbound bid requests. Nil when the
	// audience store is disabled — bid requests then simply omit user.data.
	var taxCache *taxonomyCache
	if audiencePG != nil {
		taxCache = newTaxonomyCache(context.Background(), audiencePG,
			keys.Audience.TaxonomyRefresh.Get(cfg), log)
	}

	// Readiness: placement cache must have loaded at least once. The SSP
	// can't serve bid requests without inventory metadata.
	hlth.AddReadinessCheck("placement-cache", func(_ context.Context) error {
		ts, err := placementCache.LastLoaded()
		if err != nil {
			return err
		}
		if ts.IsZero() {
			return errors.New("placement cache not yet loaded")
		}
		return nil
	})

	// Secrets warm cache — same shape DSP uses for management-endpoint auth.
	secretsCache := secrets.Start(context.Background(), cfg, clk, log, constants.ServiceSSP)
	lc.OnShutdown("secrets-cache", func(_ context.Context) error { secretsCache.Stop(); return nil })
	hlth.AddReadinessCheck("secrets-cache", func(_ context.Context) error { return secretsCache.Ready() })

	metrics := middleware.NewMetrics(constants.ServiceSSP)
	sspServePhases = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "adtech", Name: "ssp_serve_phase_duration_seconds",
		Help:    "Per-phase serve latency: pre_auction|auction|render.",
		Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5},
	}, []string{"phase"})
	metrics.Registry().MustRegister(sspServePhases)

	// Step 0 of the audience L1 cache: wrap the public-segment lookup in a shadow
	// hit-rate probe (off by default → one atomic check per lookup; counters on
	// the SSP registry). Flip audience.l1_probe_enabled live to measure.
	if audienceStore != nil {
		ap := audprobe.Wrap(audienceStore,
			config.NewLiveBool(sc.Manager, cfg, keys.Audience.L1ProbeEnabledSSP.Key(), keys.Audience.L1ProbeEnabledSSP.Default()).Value,
			config.NewLiveDuration(sc.Manager, cfg, keys.Audience.L1ProbeTTLSSP.Key(), keys.Audience.L1ProbeTTLSSP.Default()).Value,
			metrics.Registry())
		lc.OnShutdown("audience-l1-probe", func(context.Context) error { ap.Stop(); return nil })
		audienceStore = ap
	}

	mux := http.NewServeMux()
	// On-demand profiler (internal mux only; zero cost until a profile is
	// pulled). Block/mutex profiling stays off until armed — see
	// middleware.SetProfileRates.
	middleware.AttachPprof(mux)
	mux.Handle(routes.Healthz, hlth.LivenessHandler())
	mux.Handle(routes.Readyz, hlth.ReadinessHandler())
	mux.Handle(routes.Metrics, metrics.Handler())

	// Placement management. GET (list, from warm cache) and POST (create new
	// placement) share the collection route; the by-id route handles PATCH
	// and DELETE. Mirrors cmd/dsp/management.go's campaign-CRUD layout.
	dbURL := cfg.Get(keys.Database.URL.Key(), "")
	mgmtDB := openManagementDB(dbURL, log)
	if mgmtDB != nil {
		lc.OnShutdown("ssp-mgmt-db", func(ctx context.Context) error { return mgmtDB.Close() })
	}
	bus, _ := natsbus.New(keys.NATS.URL.Get(cfg), constants.ServiceSSP, log)
	// Membership freshness is owned by the single append-based cache writer
	// (cmd/pipeline); the SSP just reads the Redis sets for user.data stamping,
	// so no per-pod membership-invalidate subscription here anymore.
	// Taxonomy-label invalidates → every pod's user.data stamp map refreshes
	// in seconds (the gateway publishes on PUT /v1/api/audiences/taxonomy;
	// ingest publishes on membership writes). Without this only the pod
	// behind the debug port-forward refreshed promptly — the other replicas
	// stamped stale user.data until the 30s tick.
	if bus != nil {
		taxCache.subscribeInvalidate(context.Background(), bus)
	}
	// Data-monetization attribution publisher (nil bus → no-op).
	dfPublisher := newDataFeePublisher(bus, log)
	if bus != nil {
		lc.OnShutdown("ssp-mgmt-bus", func(ctx context.Context) error { return bus.Close() })
	}
	// Management CRUD endpoints wrap with AuthAPIKey — operators present
	// X-API-Key, validated against the secrets warm cache. The visitor-
	// facing serve endpoints below stay open since they're called by
	// browsers and need no per-call credentials (those use HMAC instead).
	auth := middleware.AuthAPIKey(secretsCache, log)
	placementsHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			all := placementCache.All()
			// Tenant read filter: a customer session (publisher via the
			// gateway) sees only its own placements; platform callers
			// (operator key without forwarded identity) see all.
			if scope := middleware.CallerScope(r); scope.Resolved && !scope.Platform {
				scoped := all[:0:0]
				for _, p := range all {
					if p.AccountID == scope.AccountID {
						scoped = append(scoped, p)
					}
				}
				all = scoped
			}
			json.NewEncoder(w).Encode(all)
		case http.MethodPost:
			if mgmtDB == nil {
				http.Error(w, "management db unavailable", http.StatusServiceUnavailable)
				return
			}
			handlePlacementCreate(w, r, mgmtDB, bus, log)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.Handle(routes.SSPPlacements, auth(placementsHandler))
	mux.Handle(routes.SSPPlacements+"/", auth(http.HandlerFunc(placementByIDHandler(mgmtDB, bus, log))))
	mux.Handle(routes.SSPPublishers, auth(http.HandlerFunc(publishersListHandler(mgmtDB, log))))

	if keys.Debug.EndpointsEnabled.Get(cfg) {
		mux.HandleFunc(routes.DebugCacheRefresh, warm.RefreshHandler(placementCache, secretsCache.Cache))
		if audiencePreloader != nil {
			mux.HandleFunc(routes.DebugAudienceRefresh, audienceRefreshHandler(audiencePreloader, taxCache, log))
		}
	}

	// Identity auto-build: publish per-request identity signals to the
	// identity-consumer, which builds graph edges. Opt-in; needs NATS.
	var idPublisher *identityPublisher
	if keys.SSP.IdentityObserveEnabled.Get(cfg) {
		idPublisher = newIdentityPublisher(bus, log)
		if idPublisher != nil {
			log.Info("ssp identity observation enabled (publishing to identity-consumer)")
		}
	}

	// Behavioural signal capture: one consent-gated request row per ad
	// request, landed in the behaviour_signals Delta table by the pipeline.
	// See behaviour.go for the capture-time consent gate.
	var bhPublisher *behaviourPublisher
	if keys.SSP.BehaviourObserveEnabled.Get(cfg) {
		bhPublisher = newBehaviourPublisher(bus, log)
		if bhPublisher != nil {
			log.Info("ssp behaviour observation enabled (publishing to behaviour_signals)")
		}
	}

	debugEnabledFn := func() bool { return keys.Debug.EndpointsEnabled.Get(cfg) }
	// Household id derivation (CTV): salted-HMAC of the client IP, config-
	// gated. Reads the knobs per call so a live disable takes effect without
	// a restart; the salt is secret-tier (env-sourced) and must match every
	// other deriver (seed, tests) — see identity.HouseholdID.
	householdFn := func(ip string) string {
		if !keys.SSP.HouseholdEnabled.Get(cfg) {
			return ""
		}
		return identity.HouseholdID(keys.SSP.HouseholdSalt.Get(cfg), ip)
	}
	// End-user IP resolution (household + identity fingerprint): shared
	// trusted-proxy parse, allowlist-gated ?ip= override. See clientip.go.
	endUserIPFn := newEndUserIPFn(cfg)
	mux.HandleFunc(routes.SSPRequest, requestAdHandler(log, placementCache, audienceStore, taxCache, exchangeURL, sellerDomain, idPublisher, bhPublisher, dfPublisher, debugEnabledFn, householdFn, endUserIPFn))
	adServerURL := keys.SSP.AdserverURL.Get(cfg)
	// Live kill switch for serving an external display winner's bid.adm
	// (wrapped with platform beacons); off = legacy placeholder behavior.
	consumeDisplayAdMFn := func() bool { return keys.SSP.ConsumeDisplayAdM.Get(cfg) }
	mux.HandleFunc(routes.SSPServe, shedOnEventPressure(log, serveAdHandler(log, placementCache, audienceStore, taxCache, exchangeURL, adServerURL, sellerDomain, idPublisher, bhPublisher, dfPublisher, debugEnabledFn, householdFn, endUserIPFn, consumeDisplayAdMFn)))

	// Per-IP rate limit on the public SSP endpoints (ratelimit_rps=0 → disabled).
	sspRL := middleware.NewLiveRateLimiter(func() middleware.RateLimitConfig {
		return middleware.RateLimitConfig{
			RPS:         keys.SSP.RateLimitRPS.Get(cfg),
			Burst:       keys.SSP.RateLimitBurst.Get(cfg),
			TrustedHops: keys.SSP.RateLimitTrustedHops.Get(cfg),
			Allowlist:   keys.SSP.RateLimitAllowlist.Get(cfg),
			Distributed: keys.SSP.RateLimitDistributed.Get(cfg),
		}
	}, log).WithDistributedBackend(constants.ServiceSSP, l2)
	handler := tracing.HTTPMiddleware(constants.ServiceSSP)(metrics.Wrap(middleware.CORS(sspRL.Wrap(mux))))
	server := &http.Server{Addr: ":" + port, Handler: handler, ReadTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second}

	log.Info("ssp starting", "port", port, "placements", placementCache.Len(), "exchange", exchangeURL)
	lifecycle.ServeHTTP(lc, server, log, 30*time.Second)
}

func startPlacementCache(cfg *config.Config, clk clock.Clock, log *slog.Logger) *warm.Cache[postgres.PlacementRow] {
	pollInterval := firstNonZeroDuration(
		cfg.GetDuration(keys.SSP.WarmPlacementsPollInterval.Key(), 0),
		keys.CacheWarm.PollInterval.Get(cfg),
	)
	loader := pickPlacementLoader(cfg, log)
	bus := connectNATS(cfg, log)
	c := warm.New(warm.Config[postgres.PlacementRow]{
		Name:              "placements",
		Loader:            loader,
		Clock:             clk,
		Bus:               bus,
		InvalidateSubject: events.SubjectCacheInvalidatePlacements,
		PollInterval:      pollInterval,
		Log:               log,
	})
	if err := c.Start(context.Background()); err != nil {
		log.Error("placement cache initial load failed", "error", err)
	}
	return c
}

// pickPlacementLoader returns a self-healing warm.Loader. Lazy-opens
// Postgres on first LoadAll and reconnects after any error so the SSP
// picks up placements automatically if Postgres was unreachable at
// boot (see also pkg/cache/warm.RetryingLoader doc comment).
func pickPlacementLoader(cfg *config.Config, log *slog.Logger) warm.Loader[postgres.PlacementRow] {
	dbURL := cfg.Get(keys.Database.URL.Key(), "")
	return &warm.RetryingLoader[postgres.PlacementRow]{
		Log:   log,
		KeyFn: func(r postgres.PlacementRow) string { return r.ID },
		Construct: func() (warm.Loader[postgres.PlacementRow], error) {
			if dbURL == "" {
				return nil, fmt.Errorf("database.url not set")
			}
			store, err := postgres.New(postgres.Config{PrimaryURL: dbURL, MaxOpenConns: 5, MaxIdleConns: 2, ConnMaxLifetime: 5 * time.Minute})
			if err != nil {
				return nil, fmt.Errorf("postgres connect: %w", err)
			}
			return &postgres.PlacementLoader{Store: store}, nil
		},
	}
}

func connectNATS(cfg *config.Config, log *slog.Logger) events.EventBus {
	url := cfg.Get(keys.SSP.NATSURL.Key(), keys.Exchange.NATSURL.Get(cfg))
	bus, err := natsbus.New(url, constants.ServiceSSP, log)
	if err != nil {
		log.Warn("nats unavailable, placement cache will poll only", "error", err)
		return nil
	}
	return bus
}

// openAudienceStore returns (Lookup, preloader-or-nil, pg-store-or-nil,
// stopFn). The preloader is non-nil only when the warm-preload variant is
// active — see DSP's matching function for the rationale (debug refresh
// endpoint). The raw pg store rides alongside whichever Lookup variant is
// chosen: the taxonomy warm cache refreshes from it off the hot path.
func openAudienceStore(cfg *config.Config, l2 cache.L2Cache, log *slog.Logger) (audstore.Lookup, *audpreload.Preloader, *audiencepg.Store, func()) {
	dbURL := cfg.Get(keys.Database.URL.Key(), "")
	if dbURL == "" {
		log.Warn("database.url not set, audience store disabled")
		return nil, nil, nil, func() {}
	}
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Warn("audience store open failed", "error", err)
		return nil, nil, nil, func() {}
	}
	// NO boot-time ping gate — sql.Open is lazy and a cold boot races
	// Postgres readiness; the ping used to permanently disable audience
	// segments (see cmd/dsp openAudienceStore). Pool connects on first
	// use; the preloader loop retries forever.
	pg := audiencepg.New(db)
	if l2 == nil {
		log.Info("audience store connected (postgres-direct, no L2 cache)")
		return pg, nil, pg, func() { _ = db.Close() }
	}
	interval := keys.Audience.PreloadInterval.Get(cfg)
	ttl := keys.Audience.CacheTTL.Get(cfg)
	pre := audpreload.New(audpreload.Config{DB: db, L2: l2, Interval: interval, TTL: ttl, Log: log})
	if err := pre.Start(context.Background()); err != nil {
		log.Warn("audience preloader start failed, falling back to lazy cache", "error", err)
		return audcached.New(pg, l2, ttl, log), nil, pg, func() { _ = db.Close() }
	}
	log.Info("audience store connected (redis warm preload)", "interval", interval, "ttl", ttl)
	return pre, pre, pg, func() { pre.Stop(); _ = db.Close() }
}

// connectRedis returns a real Redis L2 cache if reachable, falling back
// to an in-memory L2 (still useful as a per-pod LRU). Identical pattern
// to the DSP wiring so the same audience-cache path works regardless of
// whether the service is run alongside Redis or in a Redis-less unit
// test environment.
func connectRedis(cfg *config.Config, log *slog.Logger) cache.L2Cache {
	addr := keys.Redis.URL.Get(cfg)
	pwd := keys.Redis.Password.Get(cfg)
	db := keys.Redis.DB.Get(cfg)
	pool := keys.Redis.PoolSize.Get(cfg)
	// Self-healing: a failed boot dial no longer latches MemoryL2 forever —
	// the wrapper serves fail-open from memory and swaps to Redis when the
	// background retry lands (pkg/cache/selfheal.go).
	return cache.NewSelfHealingL2(func(ctx context.Context) (cache.L2Cache, error) {
		return cacheredis.New(ctx, cacheredis.Config{Addr: addr, Password: pwd, DB: db, PoolSize: pool})
	}, 10*time.Second, addr, log)
}

// auctionContext captures everything we need after the SSP runs the
// auction against the exchange. Shared by both request (X-ray) and serve
// (visitor) handlers.
type auctionContext struct {
	TraceID   string
	Placement postgres.PlacementRow
	BidResp   openrtb.BidResponse
	// HouseholdID is the derived hh: id for this request (empty when
	// household derivation is disabled or no IP was resolvable). Rides to
	// the ad server on the ServeRequest so freq caps can key per household.
	HouseholdID string
	// BidReq + DevHeaders are kept so the serve handler can RE-RUN the exchange
	// auction excluding a freq-capped winner (via BAdv) to serve the runner-up
	// instead of no-filling. The identity/behaviour/data-fee side-effects already
	// fired once in runSSPAuction; re-auctioning only re-hits the exchange.
	BidReq     openrtb.BidRequest
	DevHeaders map[string]string
}

// stampedSegments returns the public audience segments that actually rode the
// bid request — nil when the consent gate suppressed stamping (so callers echo
// exactly what left the platform, never the pre-gate lookup result).
func (ac auctionContext) stampedSegments() []string {
	if ac.BidReq.User != nil && ac.BidReq.User.Ext != nil {
		return ac.BidReq.User.Ext.Segments
	}
	return nil
}

// uuidPattern matches Postgres's canonical lowercase 8-4-4-4-12 hex UUID
// representation. Used to discriminate "looks like a real placement UUID"
// from "looks like an external key we need to derive".
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// runSSPAuction is the shared auction-execution path used by both
// requestAdHandler (X-ray) and serveAdHandler (visitor). Returns the bid
// response plus the placement row so the caller can decide how much detail
// to expose to its caller.
func runSSPAuction(w http.ResponseWriter, r *http.Request, log *slog.Logger, placements *warm.Cache[postgres.PlacementRow], audienceStore audstore.Lookup, taxCache *taxonomyCache, exchangeURL, sellerDomain string, idPublisher *identityPublisher, bhPublisher *behaviourPublisher, dfPublisher *dataFeePublisher, debugEnabledFn func() bool, householdFn func(ip string) string, endUserIPFn func(*http.Request) string) (auctionContext, bool) {
	serveStart := time.Now()
	placementExt := r.URL.Query().Get("placement_id")
	geo := r.URL.Query().Get("geo")
	device := r.URL.Query().Get("device")
	userID := r.URL.Query().Get("user_id")
	// channel lets the publisher-adserver tell us "this is a video
	// request" so we build a Video imp instead of a Banner. Defaults
	// to display when omitted so existing callers keep their shape.
	channel := r.URL.Query().Get("channel")

	if placementExt == "" {
		placementExt = "pl-news-mpu"
	}
	// Accept either a raw UUID (UI-created placements that have no external
	// key) or a friendly external_id (seed-derived). uuidPattern matches the
	// canonical 8-4-4-4-12 lowercase hex form Postgres returns; anything else
	// is treated as an external key and deterministically derived.
	var placementID string
	if uuidPattern.MatchString(placementExt) {
		placementID = placementExt
	} else {
		placementID = idgen.Derive("placement", placementExt)
	}

	p, ok := placements.ByID(placementID)
	if !ok {
		http.Error(w, "placement not found", http.StatusNotFound)
		return auctionContext{}, false
	}
	// Fall back to the placement's DECLARED channel so a publisher's DOOH / retail
	// / in-game placement serves that channel without an explicit ?channel= — the
	// query param stays an override (e.g. the simulator). Only the emerging
	// channels are placement-declared; video/audio/native ride the media object.
	if channel == "" {
		switch p.Format {
		case constants.ChannelDOOH, constants.ChannelRetail, constants.ChannelInGame:
			channel = p.Format
		}
	}

	traceID := tracing.TraceIDFromContext(r.Context())
	if traceID == "" {
		traceID = fmt.Sprintf("ssp-%d", time.Now().UnixMilli())
	}
	ctx := logger.WithTraceID(r.Context(), traceID)
	reqLog := logger.WithContext(log, ctx)

	// Effective floor: the placement's base floor raised by any device/geo/
	// daypart override in floor_config that matches this request (publisher-
	// favouring max). Empty config → base floor unchanged.
	effectiveFloor := floors.Effective(p.FloorPrice, p.FloorConfig, device, geo, time.Now())
	bidReq := openrtb.BidRequest{
		ID: traceID,
		Imp: []openrtb.Imp{{
			ID:       "imp-1",
			TagID:    p.ID,
			BidFloor: effectiveFloor,
		}},
		Site: &openrtb.Site{
			Domain:    p.PublisherDomain,
			Page:      p.PageURLPattern,
			Cat:       p.Categories,
			Keywords:  r.URL.Query().Get("keywords"), // comma-separated page keywords
			Publisher: &openrtb.Publisher{ID: p.PublisherID},
		},
		TMax: 100,
	}
	// Blocked advertiser domains (OpenRTB badv): the CTV pod path passes the
	// advertisers already selected into the pod so this sub-auction excludes
	// them, guaranteeing competitive separation instead of relying on variance.
	if badv := r.URL.Query().Get("badv"); badv != "" {
		for _, d := range strings.Split(badv, ",") {
			if d = strings.TrimSpace(d); d != "" {
				bidReq.BAdv = append(bidReq.BAdv, d)
			}
		}
	}
	switch {
	case channel == constants.ChannelDOOH:
		// Digital-out-of-home: a screen shows a banner-shaped creative (image or
		// short looping video rendered as a frame), so the imp is a Banner sized to
		// the screen; the DOOH channel rides imp.ext.channel so the exchange routes
		// it to the TimeSlot strategy. No user object — DOOH is venue/contextual, so
		// the per-user/household frequency cap simply doesn't apply. The venue's
		// audience multiplier (one play = N impressions) is applied at proof-of-play.
		bidReq.Imp[0].Banner = &openrtb.Banner{W: p.Width, H: p.Height}
		if bidReq.Imp[0].Ext == nil {
			bidReq.Imp[0].Ext = &openrtb.ImpExt{}
		}
		bidReq.Imp[0].Ext.Channel = constants.ChannelDOOH
	case channel == constants.ChannelRetail:
		// Retail media: a sponsored-product slot on a retailer's search/browse
		// page. Banner-shaped creative; imp.ext.channel=retail routes the exchange
		// to the relevance-weighted strategy, which ranks the DSP's product slate
		// by relevance × bid. The shopper's browsed/searched categories ride
		// Site.Cat — a ?cat= override lets the results page declare what's being
		// browsed (the relevance signal each sponsored product is scored against).
		bidReq.Imp[0].Banner = &openrtb.Banner{W: p.Width, H: p.Height}
		if bidReq.Imp[0].Ext == nil {
			bidReq.Imp[0].Ext = &openrtb.ImpExt{}
		}
		bidReq.Imp[0].Ext.Channel = constants.ChannelRetail
		bidReq.Imp[0].Ext.PlacementType = "sponsored_product"
		if cat := r.URL.Query().Get("cat"); cat != "" {
			bidReq.Site.Cat = strings.Split(cat, ",")
		}
		// Number of sponsored slots on the results page (placement.surfaces;
		// ?surfaces= overrides). Default 1. The exchange ranks the product slate
		// and returns the top-N; each slot bills on its own sub-trace.
		slots := 1
		if p.Surfaces > 0 {
			slots = p.Surfaces
		}
		if s := r.URL.Query().Get("surfaces"); s != "" {
			if n, err := strconv.Atoi(s); err == nil && n > 0 {
				slots = n
			}
		}
		bidReq.Imp[0].Ext.Surfaces = slots
	case channel == constants.ChannelInGame:
		// In-game intrinsic: ad surfaces embedded in a 3D scene (a stadium's
		// hoardings, a racetrack's signage). Banner-shaped creatives; imp.ext
		// channel=ingame + placement_type=intrinsic routes the exchange to the
		// Batch strategy, which fills the scene's surfaces in one auction with
		// one-advertiser / one-category-per-scene competitive separation.
		// The scene's surface count comes from the placement (surfaces column);
		// ?surfaces=N overrides it. Default 1.
		bidReq.Imp[0].Banner = &openrtb.Banner{W: p.Width, H: p.Height}
		if bidReq.Imp[0].Ext == nil {
			bidReq.Imp[0].Ext = &openrtb.ImpExt{}
		}
		bidReq.Imp[0].Ext.Channel = constants.ChannelInGame
		bidReq.Imp[0].Ext.PlacementType = "intrinsic"
		surfaces := 1
		if p.Surfaces > 0 {
			surfaces = p.Surfaces
		}
		if s := r.URL.Query().Get("surfaces"); s != "" {
			if n, err := strconv.Atoi(s); err == nil && n > 0 {
				surfaces = n
			}
		}
		bidReq.Imp[0].Ext.Surfaces = surfaces
	case channel == "video":
		// Standard pre-roll request by default (HTTP-progressive MP4, VAST 4.x,
		// 5–30s, 640x360, skippable), overridden by the placement's video_config
		// (skippability, duration window, mimes, protocols, placement type).
		bidReq.Imp[0].Video = buildVideoImp(p.VideoConfig)
	case channel == "audio":
		bidReq.Imp[0].Audio = &openrtb.Audio{
			Mimes: []string{"audio/mpeg", "audio/mp4"},
			// AdCOM ids: 9/10 = DAAST 1.0 / wrapper (advertised for
			// hypothetical third parties; retired as OUR emission format),
			// 11/13 = VAST 4.1/4.2 — modern audio is VAST 4.x audio
			// MediaFiles. The old [1,2] claimed VAST 1.0/2.0 by the
			// standard table while meaning DAAST — fixed for interop.
			Protocols:   []int{9, 10, 11, 13},
			MinDuration: 10,
			MaxDuration: 60,
			Feed:        2, // podcast
		}
	case channel == "native" || p.Format == "native":
		// Standard in-feed native request: title + main image + sponsored-by
		// (required), plus body and CTA. The OpenRTB Native request object is
		// itself JSON, carried as a string in Imp.Native.Request.
		reqJSON, err := native.MarshalRequest(native.StandardRequest(native.Spec{WantBody: true, WantCTA: true}))
		if err != nil {
			reqLog.Error("native request marshal failed", "error", err)
		}
		bidReq.Imp[0].Native = &openrtb.Native{Request: reqJSON, Ver: native.Ver}
	case p.Format == "display" || p.Format == "banner" || p.Format == "":
		bidReq.Imp[0].Banner = &openrtb.Banner{W: p.Width, H: p.Height}
	}
	if geo != "" {
		if bidReq.Device == nil {
			bidReq.Device = &openrtb.Device{}
		}
		bidReq.Device.Geo = &openrtb.Geo{Country: geo}
	}
	if device != "" {
		if bidReq.Device == nil {
			bidReq.Device = &openrtb.Device{}
		}
		bidReq.Device.DeviceType = deviceTypeInt(device)
	}
	if os := r.URL.Query().Get("os"); os != "" {
		if bidReq.Device == nil {
			bidReq.Device = &openrtb.Device{}
		}
		bidReq.Device.OS = os
	}
	// UID2 (Unified ID 2.0): a cookieless person-based identifier carried in
	// User.EIDs. Accepted from ?uid2= so a cookieless publisher page can still
	// make the user addressable. The segment lookup falls back to the UID2
	// token when there's no first-party user_id.
	uid2 := r.URL.Query().Get("uid2")
	// First-party hashed email (sha256, client-hashed per the privacy rule) — only
	// sent by the SDK AFTER personalisation consent. Used purely as an audience
	// MATCH key (CRM/first-party onboarding: a segment uploaded by hashed email
	// matches this visitor), never stamped as an EID on the outgoing request.
	hashedEmail := r.URL.Query().Get("hashed_email")
	// Household (CTV): derive the platform household id from the end user's
	// IP and carry it as an EID. The IP is resolved ONCE here — shared
	// trusted-proxy parse plus the allowlist-gated ?ip= override (see
	// clientip.go) — and reused for the identity fingerprint below, so the
	// household and the fingerprint can never disagree for one viewer.
	// Stamped like segments — the DSP enforces the consent gate before USING
	// it. A fully anonymous viewer still has a household, so this creates the
	// User object even without user_id/uid2.
	endUserIP := ""
	if endUserIPFn != nil {
		endUserIP = endUserIPFn(r)
	}
	householdID := ""
	if householdFn != nil {
		householdID = householdFn(endUserIP)
	}
	// feeSegs is the data-monetization attribution captured when (and only
	// when) fee-bearing user.data is stamped below; consumed after the
	// auction resolves.
	var feeSegs []events.DataFeeSegment
	if userID != "" || uid2 != "" || householdID != "" || hashedEmail != "" {
		user := &openrtb.User{ID: userID}
		if uid2 != "" {
			user.EIDs = []openrtb.EID{openrtb.UID2EID(uid2)}
		}
		if householdID != "" {
			user.EIDs = append(user.EIDs, openrtb.HouseholdEID(householdID))
		}
		lookupKey := openrtb.UserKey(user) // user_id, else the UID2 token
		var segs []string
		// Segment enrichment is a nice-to-have, not load-bearing for the bid
		// request — same rationale as the DSP's private-segment lookup: under
		// pool contention a queued Postgres query must degrade to "no
		// segments", never stall the auction. One shared 25ms budget covers
		// both lookups (user + household; household runs on EVERY request,
		// anonymous included, so an unbounded query here would be a hot-path
		// latency risk).
		if audienceStore != nil && (lookupKey != "" || householdID != "" || hashedEmail != "") {
			// The two lookups are independent reads, so they run CONCURRENTLY
			// under the shared 25ms budget (serialized they were worst-case
			// 2×25ms — and before the Redis client honoured ctx deadlines on
			// the wire, this block was most of pre_auction's 237ms p95).
			// Sub-phase histogram pins that decomposition on the dashboard.
			segStart := time.Now()
			segCtx, cancelSeg := context.WithTimeout(ctx, 25*time.Millisecond)
			var userSegs, hhSegs, heSegs []string
			var wg sync.WaitGroup
			if lookupKey != "" {
				wg.Add(1)
				go func() {
					defer wg.Done()
					looked, err := audienceStore.SegmentsForUser(segCtx, lookupKey)
					if err != nil {
						reqLog.Warn("segment lookup failed (degrading to none)", "user_key", lookupKey, "error", err)
					} else {
						userSegs = looked
					}
				}()
			}
			// Public household segments: same lookup, keyed by the household
			// id (audience members carry the hh: prefix). Private household
			// segments are the DSP's own lookup — mirrors the user
			// public/private split.
			if householdID != "" {
				wg.Add(1)
				go func() {
					defer wg.Done()
					looked, err := audienceStore.SegmentsForUser(segCtx, householdID)
					if err != nil {
						reqLog.Warn("household segment lookup failed (degrading to none)", "household", householdID, "error", err)
					} else {
						hhSegs = looked
					}
				}()
			}
			// First-party / CRM audience: segments onboarded by hashed email
			// (e.g. a "diamond intenders" list uploaded via /v1/api/audiences).
			// Keyed by the raw sha256 the SDK forwards, so an uploaded member and
			// this visitor line up without any identity-graph round-trip.
			if hashedEmail != "" {
				wg.Add(1)
				go func() {
					defer wg.Done()
					looked, err := audienceStore.SegmentsForUser(segCtx, hashedEmail)
					if err != nil {
						reqLog.Warn("hashed-email segment lookup failed (degrading to none)", "error", err)
					} else {
						heSegs = looked
					}
				}()
			}
			wg.Wait()
			cancelSeg()
			segs = append(userSegs, hhSegs...)
			segs = append(segs, heSegs...)
			obsServePhase("pre_auction_segments", segStart)
		}
		// Explicit ?segments= (comma-separated) lets a publisher/test pass the
		// user's public audience segments directly; unioned with any looked up.
		if q := r.URL.Query().Get("segments"); q != "" {
			for _, s := range strings.Split(q, ",") {
				if s = strings.TrimSpace(s); s != "" {
					segs = append(segs, s)
				}
			}
		}
		// User-data gate for ALL audience signal leaving the platform. Evaluated
		// once and applied to BOTH user.ext.segments and user.data (segtax): our
		// own DSP re-evaluates consent before USING segments, but an external
		// bidder can't be relied on to, so no personalisation consent (or an
		// out-of-region data-residency assertion) → no audience membership
		// (segments OR taxonomy data) rides the bid request.
		// (Previously only user.data was gated; user.ext.segments leaked public
		// audience-segment ids to external DSPs on GPC/opt-out/no-consent requests.)
		storeUserData := requestStoresUserData(r)
		if len(segs) > 0 && storeUserData {
			user.Ext = &openrtb.UserExt{Segments: segs}
		}
		// Standard-taxonomy audience data for EXTERNAL buyers: OpenRTB
		// user.data with ext.segtax=4 (IAB Audience Taxonomy 1.1). Only
		// public segments carrying a taxonomy label ride here — unlabelled
		// segments stay platform-internal on user.ext.segments.
		if storeUserData {
			if ds := taxCache.dataSegments(segs); len(ds) > 0 {
				user.Data = []openrtb.Data{{
					Name:    sellerDomain,
					Segment: ds,
					Ext:     &openrtb.DataExt{Segtax: openrtb.SegtaxIABAudience11},
				}}
				// Attribution for data monetization, captured under the SAME
				// consent gate as the stamp: these fee-bearing segments are
				// riding the request, so a win by an external buyer owes
				// their owners a fee. Held locally — never on the request.
				feeSegs = taxCache.feeSegments(segs)
			}
		}
		bidReq.User = user
	}

	// Supply chain: originate a complete one-node schain so downstream buyers
	// can verify the path — the third leg of the transparency triad alongside
	// ads.txt and sellers.json. Skipped when ssp.seller_domain is unset.
	bidReq.Source = originSChain(sellerDomain, p.PublisherID, traceID)

	// Forward consent/regulatory signals from the ad tag so the DSP's
	// privacy.Evaluate runs on real inputs. Previously the SSP set no Regs and
	// no consent, so downstream enforcement saw empty values.
	applyPrivacySignals(r, &bidReq)

	// Auto-build the identity graph: link any identifiers that co-occurred on
	// this request. No-op when observation is disabled (nil observer).
	idPublisher.Observe(r, userID, uid2, householdID, endUserIP)

	// Behavioural signal: one consent-gated request row (self-contained —
	// content categories stamped now from the placement warm cache). The
	// user key mirrors the segment lookup precedence: user_id, else UID2.
	userKey := userID
	if userKey == "" {
		userKey = uid2
	}
	bhPublisher.Observe(r, traceID, userKey, householdID, p, channel, geo, device)

	reqLog.Info("bid request generated",
		"placement", p.ID,
		"publisher", p.PublisherID,
		"floor", effectiveFloor,
		"size", fmt.Sprintf("%dx%d", p.Width, p.Height),
	)

	// Propagate the inbound X-Dev-Slow-DSPs only when debug endpoints are
	// enabled. Real publisher requests don't set this header so prod is
	// effectively unaffected, but defence-in-depth: prod with the flag
	// off won't pass it through even if a malicious upstream injects it.
	var devHeaders map[string]string
	if debugEnabledFn() {
		if v := r.Header.Get("X-Dev-Slow-DSPs"); v != "" {
			devHeaders = map[string]string{"X-Dev-Slow-DSPs": v}
		}
	}
	phaseMark := obsServePhase("pre_auction", serveStart)
	respBody, exOK := callExchange(ctx, exchangeURL, bidReq, devHeaders, reqLog)
	if !exOK {
		http.Error(w, "exchange unavailable", http.StatusBadGateway)
		return auctionContext{}, false
	}

	obsServePhase("auction", phaseMark)
	var bidResp openrtb.BidResponse
	json.Unmarshal(respBody, &bidResp)

	// Event-layer backpressure: the exchange stamps its spool pressure on
	// every response (body extension — survives the gRPC twin, which carries
	// no headers). Remember the latest reading; the front-door shed
	// middleware throttles NEW serve requests as it rises. Every response
	// updates the value, so recovery self-clears it.
	downstreamEventPressure.Store(int64(bidResp.EventPressure))

	// Data monetization: if fee-bearing audience data rode this request and
	// an EXTERNAL bidder won, publish the attribution record (fire-and-
	// forget; accrual happens at impression time in reporting).
	dfPublisher.Observe(r, traceID, p, bidResp, feeSegs)

	return auctionContext{TraceID: traceID, Placement: p, BidResp: bidResp, HouseholdID: householdID, BidReq: bidReq, DevHeaders: devHeaders}, true
}

// callExchange runs ONE auction round against the exchange (gRPC twin or OpenRTB
// HTTP, picked by URL scheme) and returns the raw response body. Extracted so the
// serve handler can re-run the auction with an updated BAdv (excluding a
// freq-capped winner) without re-firing runSSPAuction's one-time side-effects.
func callExchange(ctx context.Context, exchangeURL string, bidReq openrtb.BidRequest, devHeaders map[string]string, reqLog *slog.Logger) ([]byte, bool) {
	body, _ := json.Marshal(bidReq)
	if grpcx.IsURL(exchangeURL) {
		_, rb, err := grpcx.RunAuction(ctx, grpcx.Target(exchangeURL), body, devHeaders)
		if err != nil {
			reqLog.Error("exchange call failed", "error", err)
			return nil, false
		}
		return rb, true
	}
	exReq, err := http.NewRequestWithContext(ctx, http.MethodPost, exchangeURL+routes.OpenRTBAuction, bytes.NewReader(body))
	if err != nil {
		reqLog.Error("build exchange request", "error", err)
		return nil, false
	}
	exReq.Header.Set(constants.HeaderContentType, constants.ContentTypeJSON)
	for k, v := range devHeaders {
		exReq.Header.Set(k, v)
	}
	tracing.InjectHTTP(ctx, exReq)
	resp, err := http.DefaultClient.Do(exReq)
	if err != nil {
		reqLog.Error("exchange call failed", "error", err)
		return nil, false
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	return rb, true
}

// originSChain builds the one-node SupplyChain this platform originates as the
// seller of record for the publisher. The node's asi is this platform's domain
// (matching sellers.json) and sid is the publisher's seller id (matching its
// sellers.json entry and ads.txt account id); hp=1 as the platform handles
// payment. Returns nil when sellerDomain is unset so schain is simply omitted.
func originSChain(sellerDomain, publisherID, traceID string) *openrtb.Source {
	if sellerDomain == "" {
		return nil
	}
	return &openrtb.Source{
		FD:  1,
		TID: traceID,
		Ext: &openrtb.SourceExt{SChain: &openrtb.SupplyChain{
			Complete: 1,
			Ver:      openrtb.SChainVersion,
			Nodes: []openrtb.SupplyChainNode{{
				ASI: sellerDomain,
				SID: publisherID,
				RID: traceID,
				HP:  1,
			}},
		}},
	}
}

// applyPrivacySignals reads consent/regulatory signals from the ad-tag request
// (query params plus the Sec-GPC header) and populates Regs / User.Ext.Consent
// on the outbound bid request. This is what makes the DSP's privacy.Evaluate
// gate operate on real inputs rather than empty defaults.
//
// Global Privacy Control (Sec-GPC: 1, or ?gpc=1) is carried first-class in
// Regs.ext.gpc so the DSP can honour and log it distinctly; GPP is decoded for
// US opt-out signals downstream in pkg/privacy.
func applyPrivacySignals(r *http.Request, bidReq *openrtb.BidRequest) {
	q := r.URL.Query()
	gdpr := q.Get("gdpr")
	consent := q.Get("gdpr_consent")
	if consent == "" {
		consent = q.Get("consent")
	}
	usPrivacy := q.Get("us_privacy")
	gpp := q.Get("gpp")
	gppSID := q.Get("gpp_sid")
	dataResidency := q.Get("data_residency")

	// Global Privacy Control: a browser-level "do not sell/share" signal,
	// carried either as the Sec-GPC request header or an explicit ?gpc=1.
	var gpc int
	if r.Header.Get("Sec-GPC") == "1" || q.Get("gpc") == "1" {
		gpc = 1
	}

	var coppa int
	if q.Get("coppa") == "1" {
		coppa = 1
	}
	var gdprFlag int
	if gdpr == "1" {
		gdprFlag = 1
	}

	// Only attach Regs when at least one signal is present, so minimal bid
	// requests (and any golden-file comparisons) serialise identically.
	if gdpr != "" || usPrivacy != "" || gpp != "" || gppSID != "" || coppa == 1 || gpc == 1 || dataResidency != "" {
		bidReq.Regs = &openrtb.Regs{COPPA: coppa, Ext: &openrtb.RegsExt{
			GDPR:          gdprFlag,
			USPrivacy:     usPrivacy,
			GPP:           gpp,
			GPPSID:        gppSID,
			GPC:           gpc,
			DataResidency: dataResidency,
		}}
	}

	if consent != "" {
		if bidReq.User == nil {
			bidReq.User = &openrtb.User{}
		}
		if bidReq.User.Ext == nil {
			bidReq.User.Ext = &openrtb.UserExt{}
		}
		bidReq.User.Ext.Consent = consent
	}
}

// requestAdHandler is the X-ray endpoint: returns the raw BidResponse so
// the harness/tests can assert on auction outcomes (winner seat, clearing
// price, deal_id). A real publisher page should NOT call this — auction
// internals must not leak to the browser. The /v1/ssp/serve endpoint is
// the realistic visitor-facing path.
func requestAdHandler(log *slog.Logger, placements *warm.Cache[postgres.PlacementRow], audienceStore audstore.Lookup, taxCache *taxonomyCache, exchangeURL, sellerDomain string, idPublisher *identityPublisher, bhPublisher *behaviourPublisher, dfPublisher *dataFeePublisher, debugEnabledFn func() bool, householdFn func(ip string) string, endUserIPFn func(*http.Request) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ac, ok := runSSPAuction(w, r, log, placements, audienceStore, taxCache, exchangeURL, sellerDomain, idPublisher, bhPublisher, dfPublisher, debugEnabledFn, householdFn, endUserIPFn)
		if !ok {
			return
		}
		result := map[string]interface{}{
			"trace_id":     ac.TraceID,
			"placement_id": ac.Placement.ID,
			"publisher_id": ac.Placement.PublisherID,
			"site_domain":  ac.Placement.PublisherDomain,
			"width":        ac.Placement.Width,
			"height":       ac.Placement.Height,
			"bid_response": ac.BidResp,
		}
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		json.NewEncoder(w).Encode(result)
	}
}

// downstreamEventPressure is the exchange's last-reported event-spool fill
// (0–100), updated on every auction response. Read by shedOnEventPressure.
var downstreamEventPressure atomic.Int64

// sspServePhases is the serve-path phase histogram (pre_auction|auction|
// render). pre_auction = placement/segment/household work before the exchange
// call; auction = the exchange round-trip; render = adserve + response build.
// Set in main once the metrics registry exists; nil-checked for tests.
var sspServePhases *prometheus.HistogramVec

// obsServePhase records t→now under phase and returns a fresh mark.
func obsServePhase(phase string, t time.Time) time.Time {
	if sspServePhases != nil {
		sspServePhases.WithLabelValues(phase).Observe(time.Since(t).Seconds())
	}
	return time.Now()
}

// shedOnEventPressure is the FRONT-DOOR throttle for event-layer stress: when
// the exchange's event spool fills (NATS down/stalled), a growing fraction of
// incoming serve requests is answered with an immediate no-fill BEFORE any
// auction runs. Each shed request prevents the ~5 downstream events a full
// serve produces, so the spool drains instead of hitting its cap (where
// events would drop — the 2026-08-05 loss mode). Publishers see a normal
// no-bid and fall back to house ads; a refused fill costs one impression, an
// unrecorded one costs data integrity.
//
// Shed curve: 0 below 60% pressure, linear 0→100% across 60–90%, everything
// at 90%+.
func shedOnEventPressure(log *slog.Logger, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := int(downstreamEventPressure.Load())
		var frac float64
		switch {
		case p < 60:
			frac = 0
		case p >= 90:
			frac = 1
		default:
			frac = float64(p-60) / 30
		}
		if frac > 0 && rand.Float64() < frac {
			log.Warn("serve shed: event-layer backpressure", "pressure_pct", p, "shed_fraction", frac)
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			json.NewEncoder(w).Encode(serveAdResponse{NoBid: true})
			return
		}
		next(w, r)
	}
}

// serveAdResponse is what a visitor's ad SDK actually gets back: the
// rendered HTML and the pixel URLs to fire on render/click/viewability.
// ClearingPrice + DealID are included so the publisher-adserver can
// compare the SSP's auction outcome against external Prebid Server bids
// when fanning out to multiple demand sources. This DOES leak auction
// internals to whatever calls /v1/ssp/serve — acceptable because the only
// real callers today are publisher-adserver (trusted) and the pub sim
// (dev tool). A future cleanup would add /v1/ssp/serve-internal that
// exposes price + a public /v1/ssp/serve that hides it; out of scope now.
type serveAdResponse struct {
	TraceID        string  `json:"trace_id"`
	NoBid          bool    `json:"nobid,omitempty"`
	HTML           string  `json:"html,omitempty"`
	ImpressionURL  string  `json:"impression_url,omitempty"`
	ClickURL       string  `json:"click_url,omitempty"`
	ViewabilityURL string  `json:"viewability_url,omitempty"`
	Width          int     `json:"width,omitempty"`
	Height         int     `json:"height,omitempty"`
	ClearingPrice  float64 `json:"clearing_price,omitempty"`
	DealID         string  `json:"deal_id,omitempty"`
	// Video / audio bids skip the HTML render and ship the raw winner
	// fields the publisher-adserver needs to build VAST / DAAST. The
	// VAST builder lives in publisher-adserver (pkg/vast) rather than
	// here so the SSP stays format-agnostic.
	Channel          string `json:"channel,omitempty"`
	Geo              string `json:"geo,omitempty"`    // request geo — publisher-adserver bakes it into video/native/audio beacons
	Device           string `json:"device,omitempty"` // request device — same
	CreativeID       string `json:"creative_id,omitempty"`
	CampaignID       string `json:"campaign_id,omitempty"`
	PlacementID      string `json:"placement_id,omitempty"`
	PublisherID      string `json:"publisher_id,omitempty"`
	AdvertiserID     string `json:"advertiser_id,omitempty"`
	AdvertiserDomain string `json:"advertiser_domain,omitempty"`
	BidModel         string `json:"bid_model,omitempty"`
	Currency         string `json:"currency,omitempty"`
	DurationSeconds  int    `json:"duration_seconds,omitempty"`
	MediaURL         string `json:"media_url,omitempty"`
	// HouseholdID is the SSP-derived per-household cap key (hh:…). Returned on
	// video/audio wins so the stitcher (SSAI) can RECORD the frequency cap against
	// the right household when it actually stitches the ad — the SSP only PEEKed at
	// the serve decision (CapModePeek).
	HouseholdID string `json:"household_id,omitempty"`
	// AdM carries the winner's ad markup for native bids: the OpenRTB Native
	// response JSON the publisher-adserver renders into HTML + trackers.
	AdM string `json:"adm,omitempty"`
	// Segments echoes the consent-gated public audience segments that actually
	// rode the bid request (user.ext.segments) — a demo/observability
	// affordance so the publisher-adserver can stamp them on the
	// X-Adtech-Outcome header the demosite trace panel reads. Empty when the
	// consent gate suppressed stamping. The durable record is the exchange's
	// AuctionWinEvent; this is display convenience fed by the same data.
	Segments []string `json:"segments,omitempty"`
	// Slots is the retail sponsored-results grid: one entry per filled slot,
	// ranked (position 1 = top). The retailer's page renders its own product
	// cards and fires each slot's impression on its ImpressionID (sub-trace).
	Slots []serveAdSlot `json:"slots,omitempty"`
}

// serveAdSlot is one filled slot in a retail sponsored-results grid.
type serveAdSlot struct {
	Position      int     `json:"position"`
	CampaignID    string  `json:"campaign_id"`
	CreativeID    string  `json:"creative_id"`
	AdvertiserID  string  `json:"advertiser_id"`
	ClearingPrice float64 `json:"clearing_price"`
	// ImpressionID is the per-slot sub-trace — the page fires this slot's
	// impression on it so each slot bills independently.
	ImpressionID string `json:"impression_id"`
}

// capModeFor picks the frequency-cap mode for a non-display serve decision. The
// SSAI stitcher passes cap_defer=1 because it records the cap itself at stitch
// (impression) time, so the decision must only PEEK (never burn a slot on a
// nobid / cold conditioning-miss / manifest re-request). Every other caller (the
// publisher-adserver VAST path) has no async gap — serve ≈ impression, like
// display — so it check-and-records here.
func capModeFor(r *http.Request) string {
	if r.URL.Query().Get("cap_defer") == "1" {
		return models.CapModePeek
	}
	return models.CapModeCheckRecord
}

// doAdServe POSTs a ServeRequest to the ad server and returns its
// HTTP-equivalent status + body. It rides the gRPC twin when adServerURL is a
// grpc:// target (the envelope carries the status, so the 429 frequency-cap
// decline behaves identically on either transport), else plain HTTP. Shared by
// the display render path and the non-display cap-only check.
func doAdServe(ctx context.Context, adServerURL string, body []byte) (int, []byte, error) {
	if grpcx.IsURL(adServerURL) {
		return grpcx.ServeAd(ctx, grpcx.Target(adServerURL), body, nil)
	}
	adReq, err := http.NewRequestWithContext(ctx, http.MethodPost, adServerURL+routes.AdServe, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	adReq.Header.Set(constants.HeaderContentType, constants.ContentTypeJSON)
	tracing.InjectHTTP(ctx, adReq)
	adResp, err := http.DefaultClient.Do(adReq)
	if err != nil {
		return 0, nil, err
	}
	defer adResp.Body.Close()
	rb, _ := io.ReadAll(adResp.Body)
	return adResp.StatusCode, rb, nil
}

// serveAdHandler is the realistic publisher-visitor endpoint. The SSP runs
// the auction, picks the winner, calls the ad server internally, and
// returns just the rendered HTML + pixel URLs. The browser never learns
// who won or for how much — that's server-only knowledge in real OpenRTB.
//
// Anything the user wants to see about the auction internals (winner, fan-out,
// per-DSP latencies, NATS event consumers) shows up via Jaeger polling on
// the same trace_id, NOT via this response.
func serveAdHandler(log *slog.Logger, placements *warm.Cache[postgres.PlacementRow], audienceStore audstore.Lookup, taxCache *taxonomyCache, exchangeURL, adServerURL, sellerDomain string, idPublisher *identityPublisher, bhPublisher *behaviourPublisher, dfPublisher *dataFeePublisher, debugEnabledFn func() bool, householdFn func(ip string) string, endUserIPFn func(*http.Request) string, consumeDisplayAdMFn func() bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ac, ok := runSSPAuction(w, r, log, placements, audienceStore, taxCache, exchangeURL, sellerDomain, idPublisher, bhPublisher, dfPublisher, debugEnabledFn, householdFn, endUserIPFn)
		if !ok {
			return
		}
		renderMark := time.Now()
		defer func() { obsServePhase("render", renderMark) }()
		ctx := logger.WithTraceID(r.Context(), ac.TraceID)
		reqLog := logger.WithContext(log, ctx)

		// No winner — return nobid response. Browser would fall back to
		// house ad / no-ad / try-next-SSP.
		if ac.BidResp.NoBid || len(ac.BidResp.SeatBid) == 0 || len(ac.BidResp.SeatBid[0].Bid) == 0 {
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			json.NewEncoder(w).Encode(serveAdResponse{TraceID: ac.TraceID, NoBid: true})
			return
		}

		// Winner picked — call the ad server to render. Browser never sees
		// these IDs/prices in our response; only the resulting HTML.
		winner := ac.BidResp.SeatBid[0].Bid[0]

		// Frequency cap for NON-DISPLAY formats. Display is capped inside the ad
		// server's render call further down; video/native/audio are rendered by
		// the publisher-adserver, so without this they'd skip the cap entirely —
		// including the per-household CTV cap that exists specifically for
		// co-viewing video. Ask the ad server (the single cap authority, same
		// Redis counters) for a cap-only decision before returning the winner.
		if ch := r.URL.Query().Get("channel"); ch == constants.ChannelVideo || ch == constants.ChannelAudio || ch == constants.ChannelNative {
			capReq := models.ServeRequest{
				TraceID:     ac.TraceID,
				Channel:     ch,
				CampaignID:  winner.CID,
				CreativeID:  winner.CrID,
				PlacementID: ac.Placement.ID,
				PublisherID: ac.Placement.PublisherID,
				UserID:      r.URL.Query().Get("user_id"),
				HouseholdID: ac.HouseholdID, // co-viewing devices on one IP share the cap
				// Only the SSAI stitcher PEEKs (it passes cap_defer=1) — its impression
				// is confirmed later, at stitch, where it RECORDs against this cap, so a
				// nobid / cold conditioning-miss / manifest re-request mustn't burn a
				// slot at the decision. The direct video/audio path (publisher-adserver
				// VAST) has no such gap — serve ≈ impression, like display — so it
				// check-and-records here (default). Without this distinction the direct
				// path would never record and the video cap wouldn't enforce.
				CapMode: capModeFor(r),
			}
			capBody, _ := json.Marshal(capReq)
			if st, _, err := doAdServe(ctx, adServerURL, capBody); err != nil {
				// Fail OPEN: a cap-service blip must not black out all video/
				// native/audio serving (these paths served uncapped until now).
				// A real cap decision (429) is still honoured below.
				reqLog.Warn("freq-cap check failed; serving uncapped", "channel", ch, "error", err)
			} else if st == http.StatusTooManyRequests {
				reqLog.Debug("freq cap: non-display ad declined", "channel", ch)
				w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
				json.NewEncoder(w).Encode(serveAdResponse{TraceID: ac.TraceID, NoBid: true})
				return
			}
		}

		// Video / audio / DOOH short-circuit: no HTML to render, just return the
		// winner's fields (creative + media URL + advertiser). Video/audio → the
		// publisher-adserver builds VAST; DOOH → the screen/CMS displays the winning
		// creative and fires a proof-of-play beacon. Keeps the SSP format-agnostic.
		if ch := r.URL.Query().Get("channel"); ch == "video" || ch == "audio" || ch == constants.ChannelDOOH {
			advDomain := ""
			if len(winner.ADomain) > 0 {
				advDomain = winner.ADomain[0]
			}
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			json.NewEncoder(w).Encode(serveAdResponse{
				TraceID: ac.TraceID,
				Channel: ch,
				Geo:     r.URL.Query().Get("geo"),
				Device:  r.URL.Query().Get("device"),
				// The winner's VAST-in-adm (OpenRTB §4.3), gated on the imp's
				// advertised protocols — a mismatch drops adm (warn), the
				// MediaURL fallback still serves, and the settled win stands.
				AdM:              admForMediaServe(ac, winner, reqLog),
				CreativeID:       winner.CrID,
				CampaignID:       winner.CID,
				PlacementID:      ac.Placement.ID,
				PublisherID:      ac.Placement.PublisherID,
				AdvertiserID:     ac.BidResp.SeatBid[0].Seat,
				AdvertiserDomain: advDomain,
				BidModel:         winner.BidModel,
				ClearingPrice:    winner.Price,
				Currency:         ac.BidResp.Cur,
				Width:            winner.W,
				Height:           winner.H,
				DurationSeconds:  winner.Dur,
				MediaURL:         winner.MediaURL,
				DealID:           winner.DealID,
				HouseholdID:      ac.HouseholdID, // SSAI records the cap against this on stitch
			})
			return
		}

		// Retail sponsored-results grid: return EVERY winning slot (structured),
		// so the retailer's page renders its own product cards and fires each
		// slot's impression on its per-slot sub-trace (BidObj.ID) — each slot
		// bills independently.
		if r.URL.Query().Get("channel") == constants.ChannelRetail && len(ac.BidResp.SeatBid) > 0 {
			var slots []serveAdSlot
			for _, sb := range ac.BidResp.SeatBid {
				for _, b := range sb.Bid {
					slots = append(slots, serveAdSlot{
						Position:      len(slots) + 1,
						CampaignID:    b.CID,
						CreativeID:    b.CrID,
						AdvertiserID:  sb.Seat,
						ClearingPrice: b.Price,
						ImpressionID:  b.ID,
					})
				}
			}
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			json.NewEncoder(w).Encode(serveAdResponse{TraceID: ac.TraceID, Channel: constants.ChannelRetail, Slots: slots})
			return
		}

		// Native short-circuit: like video/audio, the SSP stays format-agnostic
		// and hands the winner's native markup (BidObj.AdM) to the publisher-
		// adserver, which parses it and renders HTML + signs the trackers.
		if r.URL.Query().Get("channel") == "native" {
			advDomain := ""
			if len(winner.ADomain) > 0 {
				advDomain = winner.ADomain[0]
			}
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			json.NewEncoder(w).Encode(serveAdResponse{
				TraceID:          ac.TraceID,
				Channel:          "native",
				Geo:              r.URL.Query().Get("geo"),
				Device:           r.URL.Query().Get("device"),
				AdM:              winner.AdM,
				CreativeID:       winner.CrID,
				CampaignID:       winner.CID,
				PlacementID:      ac.Placement.ID,
				PublisherID:      ac.Placement.PublisherID,
				AdvertiserID:     ac.BidResp.SeatBid[0].Seat,
				AdvertiserDomain: advDomain,
				BidModel:         winner.BidModel,
				ClearingPrice:    winner.Price,
				Currency:         ac.BidResp.Cur,
				DealID:           winner.DealID,
			})
			return
		}
		// Display: render via the ad server, with a bounded re-auction fallback.
		// The ad server can decline to render even after an auction win — most
		// commonly a frequency cap (429), which is a normal no-fill, not an
		// error. Rather than immediately no-filling the slot, exclude the capped
		// advertiser's domain and re-run the auction so the runner-up can win —
		// reusing the same BAdv (blocked-advertiser) mechanism the DSPs already
		// honour for competitive separation, so no exchange/DSP change is needed.
		// Bounded; falls back to an honest no-fill when no un-capped bidder
		// remains. Only callExchange re-fires (never runSSPAuction), so the SSP's
		// one-time per-request side effects (identity/behaviour/data-fee publish)
		// are NOT duplicated across rounds.
		resp := ac.BidResp
		excluded := append([]string(nil), ac.BidReq.BAdv...)
		const maxReauctions = 4
		for attempt := 0; ; attempt++ {
			winner := resp.SeatBid[0].Bid[0]
			// External display adm (OpenRTB §4.3): when the winner carries
			// HTML markup and the kill switch is on, tell the ad server we
			// can serve it ourselves if the creative is unknown (AdMFallback)
			// — the adserver still decides+records the frequency cap first.
			// Recomputed per re-auction round (a runner-up may be internal).
			winnerAdM := ""
			if consumeDisplayAdMFn() {
				winnerAdM = displayAdMForServe(winner)
			}
			serveReq := models.ServeRequest{
				TraceID:     ac.TraceID,
				CampaignID:  winner.CID,
				CreativeID:  winner.CrID,
				AdMFallback: winnerAdM != "",
				// DealID rides serve → adserver → tracker beacon (deal=) →
				// billing, where the deal's TYPE drives contract fee modifiers.
				// Was dropped here, so deal-won impressions billed as open market.
				DealID:        winner.DealID,
				PlacementID:   ac.Placement.ID,
				PublisherID:   ac.Placement.PublisherID,
				AdvertiserID:  resp.SeatBid[0].Seat,
				BidModel:      winner.BidModel,
				ClearingPrice: winner.Price,
				Currency:      resp.Cur,
				SiteDomain:    ac.Placement.PublisherDomain,
				Width:         ac.Placement.Width,
				Height:        ac.Placement.Height,
				UserID:        r.URL.Query().Get("user_id"),
				// Household cap key: co-viewing devices on one IP share a cap.
				HouseholdID: ac.HouseholdID,
				// Behavioural capture key: only under personalisation consent —
				// same gate as the SSP's own request-row capture (behaviour.go).
				BehaviourUserID: behaviourUserKey(r),
				// geo/device ride the same serve request the SSP received; bake
				// them into the tracker beacons so impression analytics carry them.
				Geo:    r.URL.Query().Get("geo"),
				Device: r.URL.Query().Get("device"),
			}
			body, _ := json.Marshal(serveReq)
			adStatus, adBody, adErr := doAdServe(ctx, adServerURL, body)
			if adErr != nil {
				reqLog.Error("ad server call failed", "error", adErr)
				http.Error(w, "ad server unavailable", http.StatusBadGateway)
				return
			}
			if adStatus == http.StatusOK {
				var sr models.ServeResponse
				if err := json.Unmarshal(adBody, &sr); err != nil {
					reqLog.Error("ad server response decode failed", "error", err)
					http.Error(w, "ad server bad response", http.StatusBadGateway)
					return
				}

				var expandedHTML string
				switch {
				case sr.ExternalAdM && winnerAdM != "":
					// External display adm: the creative is unknown to the ad
					// server, so serve the BUYER's HTML wrapped with our signed
					// beacons (impression pixel + viewability observer — the
					// same wrapper the Prebid outbound path uses). The §4.4
					// auction macros were substituted by the exchange; the
					// platform ${...} replacer below doesn't apply (external
					// HTML never carries platform macros).
					expandedHTML = adserving.WrapExternalDisplayHTML(
						winnerAdM, sr.ImpressionURL, sr.ViewabilityURL, "data-external-adm-wrapper")
					reqLog.Info("external display adm served",
						"seat", resp.SeatBid[0].Seat, "crid", winner.CrID)
				case sr.ExternalAdM:
					// Defensive: the ad server only flags ExternalAdM when WE
					// sent AdMFallback, so this shouldn't happen — treat as a
					// render decline rather than serving an empty ad.
					reqLog.Warn("adserver flagged external adm but no adm held — no-fill")
					w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
					json.NewEncoder(w).Encode(serveAdResponse{TraceID: ac.TraceID, NoBid: true})
					return
				default:
					// Macro substitution. Creative HTML can carry standard ad-tech
					// tokens (${IMP_PIXEL}, ${CLICK_URL}, ${VIEWABILITY_URL}) so the
					// same row in the creatives table serves any auction outcome. SSP
					// substitutes them with the per-auction signed URLs before the
					// HTML reaches the browser — clients should never see raw macros.
					expandedHTML = strings.NewReplacer(
						"${IMP_PIXEL}", sr.ImpressionURL,
						"${CLICK_URL}", sr.ClickURL,
						"${VIEWABILITY_URL}", sr.ViewabilityURL,
						"${TRACE_ID}", ac.TraceID,
					).Replace(sr.HTML)
				}

				w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
				json.NewEncoder(w).Encode(serveAdResponse{
					TraceID:        ac.TraceID,
					HTML:           expandedHTML,
					ImpressionURL:  sr.ImpressionURL,
					ClickURL:       sr.ClickURL,
					ViewabilityURL: sr.ViewabilityURL,
					Width:          ac.Placement.Width,
					Height:         ac.Placement.Height,
					ClearingPrice:  winner.Price,
					DealID:         winner.DealID,
					Segments:       ac.stampedSegments(),
				})
				return
			}
			// Non-200. Only a frequency cap (429) is worth re-auctioning around;
			// any other decline is a genuine no-fill (don't JSON-decode the
			// plain-text error body — "frequency cap exceeded" parses as a bad
			// `false`, which produced spurious "decode failed" ERRORs + 502s).
			if adStatus != http.StatusTooManyRequests {
				reqLog.Warn("ad server declined to render", "status", adStatus)
				break
			}
			reqLog.Debug("ad server declined: frequency cap", "status", adStatus, "attempt", attempt)
			// Out of re-auction budget, or the capped winner carries no domain
			// to exclude (can't express the exclusion as BAdv) → honest no-fill.
			if attempt >= maxReauctions || len(winner.ADomain) == 0 {
				break
			}
			excluded = append(excluded, winner.ADomain[0])
			reBid := ac.BidReq
			reBid.BAdv = excluded
			rb, exOK := callExchange(ctx, exchangeURL, reBid, ac.DevHeaders, reqLog)
			if !exOK {
				break
			}
			var reResp openrtb.BidResponse
			json.Unmarshal(rb, &reResp)
			if reResp.NoBid || len(reResp.SeatBid) == 0 || len(reResp.SeatBid[0].Bid) == 0 {
				reqLog.Debug("re-auction after freq cap: no un-capped runner-up", "excluded", excluded)
				break
			}
			resp = reResp
			reqLog.Debug("re-auction after freq cap: runner-up won",
				"excluded", excluded, "advertiser", resp.SeatBid[0].Seat)
		}
		// No fillable winner remained after the capped advertiser(s) were
		// excluded — same honest no-fill the no-winner path returns.
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		json.NewEncoder(w).Encode(serveAdResponse{TraceID: ac.TraceID, NoBid: true})
		return
	}
}

func deviceTypeInt(s string) int {
	switch s {
	case "mobile":
		return 1
	case "desktop":
		return 2
	case "ctv":
		return 3
	case "tablet":
		return 5
	default:
		return 2
	}
}

// audienceRefreshHandler — same shape as the DSP version. Forces a sync
// preload so e2e tests' segment inserts are visible without waiting for
// the 30s tick. Also refreshes the taxonomy warm map (nil-safe) — a test
// that labels a segment needs the user.data stamp on the next bid, same
// freshness contract as memberships. Gated by debug.endpoints_enabled at
// the caller.
func audienceRefreshHandler(pre *audpreload.Preloader, taxCache *taxonomyCache, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		start := time.Now()
		if err := pre.Refresh(ctx); err != nil {
			log.Warn("audience refresh failed", "error", err)
			http.Error(w, "refresh failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		taxCache.refresh(ctx)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"refreshed":true,"duration_ms":` +
			strconv.FormatInt(time.Since(start).Milliseconds(), 10) + `}`))
	}
}

func firstNonZeroDuration(ds ...time.Duration) time.Duration {
	for _, d := range ds {
		if d > 0 {
			return d
		}
	}
	return 30 * time.Second
}
