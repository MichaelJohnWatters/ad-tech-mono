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
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	audstore "github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store"
	audcached "github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store/cached"
	audiencepg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store/postgres"
	audpreload "github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store/preload"
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
	audienceStore, audiencePreloader, audienceStop := openAudienceStore(cfg, l2, log)
	lc.OnShutdown("audience-store", func(_ context.Context) error { audienceStop(); return nil })

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

	mux := http.NewServeMux()
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
			mux.HandleFunc(routes.DebugAudienceRefresh, audienceRefreshHandler(audiencePreloader, log))
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
	mux.HandleFunc(routes.SSPRequest, requestAdHandler(log, placementCache, audienceStore, exchangeURL, sellerDomain, idPublisher, debugEnabledFn, householdFn))
	adServerURL := keys.SSP.AdserverURL.Get(cfg)
	mux.HandleFunc(routes.SSPServe, serveAdHandler(log, placementCache, audienceStore, exchangeURL, adServerURL, sellerDomain, idPublisher, debugEnabledFn, householdFn))

	handler := tracing.HTTPMiddleware(constants.ServiceSSP)(metrics.Wrap(middleware.CORS(mux)))
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

// openAudienceStore returns (Lookup, preloader-or-nil, stopFn). The
// preloader is non-nil only when the warm-preload variant is active —
// see DSP's matching function for the rationale (debug refresh endpoint).
func openAudienceStore(cfg *config.Config, l2 cache.L2Cache, log *slog.Logger) (audstore.Lookup, *audpreload.Preloader, func()) {
	dbURL := cfg.Get(keys.Database.URL.Key(), "")
	if dbURL == "" {
		log.Warn("database.url not set, audience store disabled")
		return nil, nil, func() {}
	}
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Warn("audience store open failed", "error", err)
		return nil, nil, func() {}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		log.Warn("audience store ping failed", "error", err)
		_ = db.Close()
		return nil, nil, func() {}
	}
	if l2 == nil {
		log.Info("audience store connected (postgres-direct, no L2 cache)")
		return audiencepg.New(db), nil, func() { _ = db.Close() }
	}
	interval := keys.Audience.PreloadInterval.Get(cfg)
	ttl := keys.Audience.CacheTTL.Get(cfg)
	pre := audpreload.New(audpreload.Config{DB: db, L2: l2, Interval: interval, TTL: ttl, Log: log})
	if err := pre.Start(context.Background()); err != nil {
		log.Warn("audience preloader start failed, falling back to lazy cache", "error", err)
		return audcached.New(audiencepg.New(db), l2, ttl, log), nil, func() { _ = db.Close() }
	}
	log.Info("audience store connected (redis warm preload)", "interval", interval, "ttl", ttl)
	return pre, pre, func() { pre.Stop(); _ = db.Close() }
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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, err := cacheredis.New(ctx, cacheredis.Config{Addr: addr, Password: pwd, DB: db})
	if err != nil {
		log.Warn("redis unreachable, falling back to in-memory L2", "addr", addr, "error", err)
		return cache.NewMemoryL2()
	}
	log.Info("redis connected", "addr", addr)
	return client
}

// auctionContext captures everything we need after the SSP runs the
// auction against the exchange. Shared by both request (X-ray) and serve
// (visitor) handlers.
type auctionContext struct {
	TraceID   string
	Placement postgres.PlacementRow
	BidResp   openrtb.BidResponse
}

// uuidPattern matches Postgres's canonical lowercase 8-4-4-4-12 hex UUID
// representation. Used to discriminate "looks like a real placement UUID"
// from "looks like an external key we need to derive".
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// runSSPAuction is the shared auction-execution path used by both
// requestAdHandler (X-ray) and serveAdHandler (visitor). Returns the bid
// response plus the placement row so the caller can decide how much detail
// to expose to its caller.
func runSSPAuction(w http.ResponseWriter, r *http.Request, log *slog.Logger, placements *warm.Cache[postgres.PlacementRow], audienceStore audstore.Lookup, exchangeURL, sellerDomain string, idPublisher *identityPublisher, debugEnabledFn func() bool, householdFn func(ip string) string) (auctionContext, bool) {
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
	switch {
	case channel == "video":
		// Standard pre-roll request by default (HTTP-progressive MP4, VAST 4.x,
		// 5–30s, 640x360, skippable), overridden by the placement's video_config
		// (skippability, duration window, mimes, protocols, placement type).
		bidReq.Imp[0].Video = buildVideoImp(p.VideoConfig)
	case channel == "audio":
		bidReq.Imp[0].Audio = &openrtb.Audio{
			Mimes:       []string{"audio/mpeg", "audio/mp4"},
			Protocols:   []int{1, 2}, // DAAST 1.0, DAAST 1.0 wrapper
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
	// Household (CTV): derive the platform household id from the end user's
	// IP and carry it as an EID. Same IP precedence as the identity
	// fingerprint: explicit ?ip= first (the ad tag forwards the device IP —
	// the SSP otherwise sees the publisher server or LB address), then the
	// connection-derived client IP. Stamped like segments — the DSP enforces
	// the consent gate before USING it. A fully anonymous viewer still has a
	// household, so this creates the User object even without user_id/uid2.
	householdID := ""
	if householdFn != nil {
		endUserIP := r.URL.Query().Get("ip")
		if endUserIP == "" {
			endUserIP = clientIP(r)
		}
		householdID = householdFn(endUserIP)
	}
	if userID != "" || uid2 != "" || householdID != "" {
		user := &openrtb.User{ID: userID}
		if uid2 != "" {
			user.EIDs = []openrtb.EID{openrtb.UID2EID(uid2)}
		}
		if householdID != "" {
			user.EIDs = append(user.EIDs, openrtb.HouseholdEID(householdID))
		}
		lookupKey := openrtb.UserKey(user) // user_id, else the UID2 token
		var segs []string
		if audienceStore != nil && lookupKey != "" {
			looked, err := audienceStore.SegmentsForUser(ctx, lookupKey)
			if err != nil {
				reqLog.Warn("segment lookup failed", "user_key", lookupKey, "error", err)
			} else {
				segs = looked
			}
		}
		// Public household segments: same lookup, keyed by the household id
		// (audience members carry the hh: prefix). Private household segments
		// are the DSP's own lookup — mirrors the user public/private split.
		if audienceStore != nil && householdID != "" {
			looked, err := audienceStore.SegmentsForUser(ctx, householdID)
			if err != nil {
				reqLog.Warn("household segment lookup failed", "household", householdID, "error", err)
			} else {
				segs = append(segs, looked...)
			}
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
		if len(segs) > 0 {
			user.Ext = &openrtb.UserExt{Segments: segs}
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
	idPublisher.Observe(r, userID, uid2, householdID)

	reqLog.Info("bid request generated",
		"placement", p.ID,
		"publisher", p.PublisherID,
		"floor", effectiveFloor,
		"size", fmt.Sprintf("%dx%d", p.Width, p.Height),
	)

	body, _ := json.Marshal(bidReq)
	exReq, err := http.NewRequestWithContext(ctx, http.MethodPost, exchangeURL+routes.OpenRTBAuction, bytes.NewReader(body))
	if err != nil {
		reqLog.Error("build exchange request", "error", err)
		http.Error(w, "exchange request build failed", http.StatusInternalServerError)
		return auctionContext{}, false
	}
	exReq.Header.Set(constants.HeaderContentType, constants.ContentTypeJSON)
	// Propagate the inbound X-Dev-Slow-DSPs only when debug endpoints are
	// enabled. Real publisher requests don't set this header so prod is
	// effectively unaffected, but defence-in-depth: prod with the flag
	// off won't pass it through even if a malicious upstream injects it.
	if debugEnabledFn() {
		if v := r.Header.Get("X-Dev-Slow-DSPs"); v != "" {
			exReq.Header.Set("X-Dev-Slow-DSPs", v)
		}
	}
	tracing.InjectHTTP(ctx, exReq)
	resp, err := http.DefaultClient.Do(exReq)
	if err != nil {
		reqLog.Error("exchange call failed", "error", err)
		http.Error(w, "exchange unavailable", http.StatusBadGateway)
		return auctionContext{}, false
	}
	defer resp.Body.Close()

	var bidResp openrtb.BidResponse
	json.NewDecoder(resp.Body).Decode(&bidResp)

	return auctionContext{TraceID: traceID, Placement: p, BidResp: bidResp}, true
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
	if gdpr != "" || usPrivacy != "" || gpp != "" || gppSID != "" || coppa == 1 || gpc == 1 {
		bidReq.Regs = &openrtb.Regs{COPPA: coppa, Ext: &openrtb.RegsExt{
			GDPR:      gdprFlag,
			USPrivacy: usPrivacy,
			GPP:       gpp,
			GPPSID:    gppSID,
			GPC:       gpc,
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
func requestAdHandler(log *slog.Logger, placements *warm.Cache[postgres.PlacementRow], audienceStore audstore.Lookup, exchangeURL, sellerDomain string, idPublisher *identityPublisher, debugEnabledFn func() bool, householdFn func(ip string) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ac, ok := runSSPAuction(w, r, log, placements, audienceStore, exchangeURL, sellerDomain, idPublisher, debugEnabledFn, householdFn)
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
	// AdM carries the winner's ad markup for native bids: the OpenRTB Native
	// response JSON the publisher-adserver renders into HTML + trackers.
	AdM string `json:"adm,omitempty"`
}

// serveAdHandler is the realistic publisher-visitor endpoint. The SSP runs
// the auction, picks the winner, calls the ad server internally, and
// returns just the rendered HTML + pixel URLs. The browser never learns
// who won or for how much — that's server-only knowledge in real OpenRTB.
//
// Anything the user wants to see about the auction internals (winner, fan-out,
// per-DSP latencies, NATS event consumers) shows up via Jaeger polling on
// the same trace_id, NOT via this response.
func serveAdHandler(log *slog.Logger, placements *warm.Cache[postgres.PlacementRow], audienceStore audstore.Lookup, exchangeURL, adServerURL, sellerDomain string, idPublisher *identityPublisher, debugEnabledFn func() bool, householdFn func(ip string) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ac, ok := runSSPAuction(w, r, log, placements, audienceStore, exchangeURL, sellerDomain, idPublisher, debugEnabledFn, householdFn)
		if !ok {
			return
		}
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

		// Video / audio short-circuit: no HTML to render, just return
		// the winner's media URL + duration + advertiser fields so the
		// publisher-adserver can build VAST. Tracker URLs are signed
		// inside publisher-adserver too, not here — keeps the SSP
		// format-agnostic and avoids duplicating the macros plumbing.
		if ch := r.URL.Query().Get("channel"); ch == "video" || ch == "audio" {
			advDomain := ""
			if len(winner.ADomain) > 0 {
				advDomain = winner.ADomain[0]
			}
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			json.NewEncoder(w).Encode(serveAdResponse{
				TraceID:          ac.TraceID,
				Channel:          ch,
				Geo:              r.URL.Query().Get("geo"),
				Device:           r.URL.Query().Get("device"),
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
			})
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
		serveReq := models.ServeRequest{
			TraceID:       ac.TraceID,
			CampaignID:    winner.CID,
			CreativeID:    winner.CrID,
			PlacementID:   ac.Placement.ID,
			PublisherID:   ac.Placement.PublisherID,
			AdvertiserID:  ac.BidResp.SeatBid[0].Seat,
			BidModel:      winner.BidModel,
			ClearingPrice: winner.Price,
			Currency:      ac.BidResp.Cur,
			SiteDomain:    ac.Placement.PublisherDomain,
			Width:         ac.Placement.Width,
			Height:        ac.Placement.Height,
			UserID:        r.URL.Query().Get("user_id"),
			// geo/device ride the same serve request the SSP received; bake
			// them into the tracker beacons so impression analytics carry them.
			Geo:    r.URL.Query().Get("geo"),
			Device: r.URL.Query().Get("device"),
		}
		body, _ := json.Marshal(serveReq)
		adReq, err := http.NewRequestWithContext(ctx, http.MethodPost, adServerURL+routes.AdServe, bytes.NewReader(body))
		if err != nil {
			reqLog.Error("build ad server request", "error", err)
			http.Error(w, "ad server request build failed", http.StatusInternalServerError)
			return
		}
		adReq.Header.Set(constants.HeaderContentType, constants.ContentTypeJSON)
		tracing.InjectHTTP(ctx, adReq)
		adResp, err := http.DefaultClient.Do(adReq)
		if err != nil {
			reqLog.Error("ad server call failed", "error", err)
			http.Error(w, "ad server unavailable", http.StatusBadGateway)
			return
		}
		defer adResp.Body.Close()
		// The ad server can decline to render even after an auction win — most
		// commonly a frequency cap (429), which is a normal no-fill, not an
		// error. Treat any non-200 as an unfilled opportunity and return the
		// same nobid response the no-winner path uses, instead of JSON-decoding
		// a plain-text error body (which produced spurious "decode failed"
		// ERRORs + 502s: "frequency cap exceeded" parses as a bad `false`).
		if adResp.StatusCode != http.StatusOK {
			if adResp.StatusCode == http.StatusTooManyRequests {
				reqLog.Debug("ad server declined: frequency cap", "status", adResp.StatusCode)
			} else {
				reqLog.Warn("ad server declined to render", "status", adResp.StatusCode)
			}
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			json.NewEncoder(w).Encode(serveAdResponse{TraceID: ac.TraceID, NoBid: true})
			return
		}
		var sr models.ServeResponse
		if err := json.NewDecoder(adResp.Body).Decode(&sr); err != nil {
			reqLog.Error("ad server response decode failed", "error", err)
			http.Error(w, "ad server bad response", http.StatusBadGateway)
			return
		}

		// Macro substitution. Creative HTML can carry standard ad-tech tokens
		// (${IMP_PIXEL}, ${CLICK_URL}, ${VIEWABILITY_URL}) so the same row
		// in the creatives table serves any auction outcome. SSP substitutes
		// them with the per-auction signed URLs before the HTML reaches the
		// browser — clients should never see raw macros.
		expandedHTML := strings.NewReplacer(
			"${IMP_PIXEL}", sr.ImpressionURL,
			"${CLICK_URL}", sr.ClickURL,
			"${VIEWABILITY_URL}", sr.ViewabilityURL,
			"${TRACE_ID}", ac.TraceID,
		).Replace(sr.HTML)

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
		})
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
// the 30s tick. Gated by debug.endpoints_enabled at the caller.
func audienceRefreshHandler(pre *audpreload.Preloader, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		start := time.Now()
		if err := pre.Refresh(ctx); err != nil {
			log.Warn("audience refresh failed", "error", err)
			http.Error(w, "refresh failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
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
