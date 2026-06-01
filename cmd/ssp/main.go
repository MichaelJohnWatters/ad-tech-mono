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
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events/natsbus"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/health"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/idgen"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
	_ "github.com/lib/pq"
)

func main() {
	clk := clock.Real{}
	log := logger.New(constants.ServiceSSP)
	sc := config.Setup(constants.ServiceSSP, log)
	config.PublishSchemaWithURL(sc.Cfg.Get("database.url", ""), constants.ServiceSSP, sspSchema, log)
	cfg := sc.Cfg
	_ = sc
	hlth := health.New()
	lc := lifecycle.New(log)

	port := cfg.Get("ssp.port", routes.PortSSP)
	exchangeURL := cfg.Get("ssp.exchange_url", routes.DefaultExchangeURL)

	// OTel — required so HTTPMiddleware's server span has a real trace ID
	// that flows into logs / NATS events / analytics store.
	otelShutdown := tracing.Init(context.Background(), tracing.Config{
		ServiceName:    constants.ServiceSSP,
		ServiceVersion: cfg.Get("otel.service_version", "dev"),
		Endpoint:       cfg.Get("otel.endpoint", "localhost:4318"),
		SampleRatio:    cfg.GetFloat("otel.sample_ratio", 1.0),
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
	audienceStore, audienceStop := openAudienceStore(cfg, l2, log)
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

	metrics := middleware.NewMetrics(constants.ServiceSSP)

	mux := http.NewServeMux()
	mux.Handle(routes.Healthz, hlth.LivenessHandler())
	mux.Handle(routes.Readyz, hlth.ReadinessHandler())
	mux.Handle(routes.Metrics, metrics.Handler())

	// Placement management. GET (list, from warm cache) and POST (create new
	// placement) share the collection route; the by-id route handles PATCH
	// and DELETE. Mirrors cmd/dsp/management.go's campaign-CRUD layout.
	dbURL := cfg.Get("database.url", "")
	mgmtDB := openManagementDB(dbURL, log)
	if mgmtDB != nil {
		lc.OnShutdown("ssp-mgmt-db", func(ctx context.Context) error { return mgmtDB.Close() })
	}
	bus, _ := natsbus.New(cfg.Get("nats.url", routes.DefaultNATSURL), constants.ServiceSSP, log)
	if bus != nil {
		lc.OnShutdown("ssp-mgmt-bus", func(ctx context.Context) error { return bus.Close() })
	}
	mux.HandleFunc(routes.SSPPlacements, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			json.NewEncoder(w).Encode(placementCache.All())
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
	mux.HandleFunc(routes.SSPPlacements+"/", placementByIDHandler(mgmtDB, bus, log))
	mux.HandleFunc(routes.SSPPublishers, publishersListHandler(mgmtDB, log))

	if cfg.GetBool("debug.endpoints_enabled", true) {
		mux.HandleFunc(routes.DebugCacheRefresh, warm.RefreshHandler(placementCache))
	}

	mux.HandleFunc(routes.SSPRequest, requestAdHandler(log, placementCache, audienceStore, exchangeURL))
	adServerURL := cfg.Get("ssp.adserver_url", routes.DefaultAdServerURL)
	mux.HandleFunc(routes.SSPServe, serveAdHandler(log, placementCache, audienceStore, exchangeURL, adServerURL))

	handler := tracing.HTTPMiddleware(constants.ServiceSSP)(metrics.Wrap(middleware.CORS(mux)))
	server := &http.Server{Addr: ":" + port, Handler: handler, ReadTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second}

	log.Info("ssp starting", "port", port, "placements", placementCache.Len(), "exchange", exchangeURL)
	lifecycle.ServeHTTP(lc, server, log, 30*time.Second)
}

func startPlacementCache(cfg *config.Config, clk clock.Clock, log *slog.Logger) *warm.Cache[postgres.PlacementRow] {
	pollInterval := firstNonZeroDuration(
		cfg.GetDuration("cache.warm.placements.poll_interval", 0),
		cfg.GetDuration("cache.warm.poll_interval", 30*time.Second),
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

func pickPlacementLoader(cfg *config.Config, log *slog.Logger) warm.Loader[postgres.PlacementRow] {
	dbURL := cfg.Get("database.url", "")
	if dbURL == "" {
		log.Warn("database.url not set, placement cache will be empty")
		return emptyPlacementLoader{}
	}
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Warn("postgres open failed", "error", err)
		return emptyPlacementLoader{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		log.Warn("postgres ping failed", "error", err)
		_ = db.Close()
		return emptyPlacementLoader{}
	}
	store, _ := postgres.New(postgres.Config{PrimaryURL: dbURL, MaxOpenConns: 5, MaxIdleConns: 2, ConnMaxLifetime: 5 * time.Minute})
	log.Info("postgres connected for placement loader")
	return &postgres.PlacementLoader{Store: store}
}

type emptyPlacementLoader struct{}

func (emptyPlacementLoader) LoadAll(_ context.Context) ([]postgres.PlacementRow, error) {
	return nil, nil
}
func (emptyPlacementLoader) KeyOf(r postgres.PlacementRow) string { return r.ID }

func connectNATS(cfg *config.Config, log *slog.Logger) events.EventBus {
	url := cfg.Get("ssp.nats_url", cfg.Get("exchange.nats_url", routes.DefaultNATSURL))
	bus, err := natsbus.New(url, constants.ServiceSSP, log)
	if err != nil {
		log.Warn("nats unavailable, placement cache will poll only", "error", err)
		return nil
	}
	return bus
}

// openAudienceStore returns a Lookup for public segments. Same pattern as
// the DSP wiring: warm Redis preloader when both DB + L2 are reachable,
// lazy cache when preload fails, postgres-direct without L2, nil otherwise.
// Returns the Lookup + a Stop function the caller registers on shutdown.
func openAudienceStore(cfg *config.Config, l2 cache.L2Cache, log *slog.Logger) (audstore.Lookup, func()) {
	dbURL := cfg.Get("database.url", "")
	if dbURL == "" {
		log.Warn("database.url not set, audience store disabled")
		return nil, func() {}
	}
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Warn("audience store open failed", "error", err)
		return nil, func() {}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		log.Warn("audience store ping failed", "error", err)
		_ = db.Close()
		return nil, func() {}
	}
	if l2 == nil {
		log.Info("audience store connected (postgres-direct, no L2 cache)")
		return audiencepg.New(db), func() { _ = db.Close() }
	}
	interval := cfg.GetDuration("audience.preload_interval", 30*time.Second)
	ttl := cfg.GetDuration("audience.cache_ttl", 90*time.Second)
	pre := audpreload.New(audpreload.Config{DB: db, L2: l2, Interval: interval, TTL: ttl, Log: log})
	if err := pre.Start(context.Background()); err != nil {
		log.Warn("audience preloader start failed, falling back to lazy cache", "error", err)
		return audcached.New(audiencepg.New(db), l2, ttl, log), func() { _ = db.Close() }
	}
	log.Info("audience store connected (redis warm preload)", "interval", interval, "ttl", ttl)
	return pre, func() { pre.Stop(); _ = db.Close() }
}

// connectRedis returns a real Redis L2 cache if reachable, falling back
// to an in-memory L2 (still useful as a per-pod LRU). Identical pattern
// to the DSP wiring so the same audience-cache path works regardless of
// whether the service is run alongside Redis or in a Redis-less unit
// test environment.
func connectRedis(cfg *config.Config, log *slog.Logger) cache.L2Cache {
	addr := cfg.Get("redis.url", "localhost:6379")
	pwd := cfg.Get("redis.password", "")
	db := cfg.GetInt("redis.db", 0)
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
func runSSPAuction(w http.ResponseWriter, r *http.Request, log *slog.Logger, placements *warm.Cache[postgres.PlacementRow], audienceStore audstore.Lookup, exchangeURL string) (auctionContext, bool) {
	placementExt := r.URL.Query().Get("placement_id")
	geo := r.URL.Query().Get("geo")
	device := r.URL.Query().Get("device")
	userID := r.URL.Query().Get("user_id")

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

	bidReq := openrtb.BidRequest{
		ID: traceID,
		Imp: []openrtb.Imp{{
			ID:       "imp-1",
			TagID:    p.ID,
			BidFloor: p.FloorPrice,
		}},
		Site: &openrtb.Site{
			Domain:    p.PublisherDomain,
			Page:      p.PageURLPattern,
			Cat:       p.Categories,
			Publisher: &openrtb.Publisher{ID: p.PublisherID},
		},
		TMax: 100,
	}
	if p.Format == "display" || p.Format == "banner" || p.Format == "" {
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
	if userID != "" {
		user := &openrtb.User{ID: userID}
		if audienceStore != nil {
			segs, err := audienceStore.SegmentsForUser(ctx, userID)
			if err != nil {
				reqLog.Warn("segment lookup failed", "user_id", userID, "error", err)
			} else if len(segs) > 0 {
				user.Ext = &openrtb.UserExt{Segments: segs}
			}
		}
		bidReq.User = user
	}

	reqLog.Info("bid request generated",
		"placement", p.ID,
		"publisher", p.PublisherID,
		"floor", p.FloorPrice,
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
	// Propagate the inbound X-Dev-Slow-DSPs so the pub sim's dev toggle
	// reaches the exchange (and on to specific DSPs). Real publisher
	// requests don't set this header so prod is unaffected.
	if v := r.Header.Get("X-Dev-Slow-DSPs"); v != "" {
		exReq.Header.Set("X-Dev-Slow-DSPs", v)
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

// requestAdHandler is the X-ray endpoint: returns the raw BidResponse so
// the harness/tests can assert on auction outcomes (winner seat, clearing
// price, deal_id). A real publisher page should NOT call this — auction
// internals must not leak to the browser. The /v1/ssp/serve endpoint is
// the realistic visitor-facing path.
func requestAdHandler(log *slog.Logger, placements *warm.Cache[postgres.PlacementRow], audienceStore audstore.Lookup, exchangeURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ac, ok := runSSPAuction(w, r, log, placements, audienceStore, exchangeURL)
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
// Deliberately omits winner seat, clearing price, campaign id — those are
// competitive-info that real OpenRTB never leaks to the browser. The
// trace_id is included so the dev tool can pivot into Jaeger/Loki for the
// full server-side picture.
type serveAdResponse struct {
	TraceID        string `json:"trace_id"`
	NoBid          bool   `json:"nobid,omitempty"`
	HTML           string `json:"html,omitempty"`
	ImpressionURL  string `json:"impression_url,omitempty"`
	ClickURL       string `json:"click_url,omitempty"`
	ViewabilityURL string `json:"viewability_url,omitempty"`
	Width          int    `json:"width,omitempty"`
	Height         int    `json:"height,omitempty"`
}

// serveAdHandler is the realistic publisher-visitor endpoint. The SSP runs
// the auction, picks the winner, calls the ad server internally, and
// returns just the rendered HTML + pixel URLs. The browser never learns
// who won or for how much — that's server-only knowledge in real OpenRTB.
//
// Anything the user wants to see about the auction internals (winner, fan-out,
// per-DSP latencies, NATS event consumers) shows up via Jaeger polling on
// the same trace_id, NOT via this response.
func serveAdHandler(log *slog.Logger, placements *warm.Cache[postgres.PlacementRow], audienceStore audstore.Lookup, exchangeURL, adServerURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ac, ok := runSSPAuction(w, r, log, placements, audienceStore, exchangeURL)
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
		serveReq := models.ServeRequest{
			TraceID:       ac.TraceID,
			CampaignID:    winner.CID,
			CreativeID:    winner.CrID,
			PlacementID:   ac.Placement.ID,
			PublisherID:   ac.Placement.PublisherID,
			AdvertiserID:  ac.BidResp.SeatBid[0].Seat,
			ClearingPrice: winner.Price,
			Currency:      ac.BidResp.Cur,
			SiteDomain:    ac.Placement.PublisherDomain,
			Width:         ac.Placement.Width,
			Height:        ac.Placement.Height,
			UserID:        r.URL.Query().Get("user_id"),
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

func firstNonZeroDuration(ds ...time.Duration) time.Duration {
	for _, d := range ds {
		if d > 0 {
			return d
		}
	}
	return 30 * time.Second
}
