// cmd/adserver serves ad creatives to end-user browsers.
// Generates tracking URLs with signed parameters via macro substitution.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache"
	cacheredis "github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache/redis"
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
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/optimise"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/secrets"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
	_ "github.com/lib/pq"
)

// AdCreative is the rendered creative the ad server returns. Its source is
// the postgres-backed warm cache (metadata) plus html_content from the row
// or a body fetched from Minio at asset_url.
type AdCreative struct {
	ID         string
	Name       string
	HTML       string
	Width      int
	Height     int
	Format     string
	LandingURL string
}

func main() {
	clk := clock.Real{}
	log := logger.New(constants.ServiceAdServer)
	sc := config.Setup(constants.ServiceAdServer, keys.AdServerSchema(), log)
	cfg := sc.Cfg
	knobs := NewKnobs(sc)
	hlth := health.New()
	lc := lifecycle.New(log)

	// Sign the impression/click/view pixels baked into served ad HTML with the
	// ACTIVE hmac_tracker key from the secrets store (Phase I) — so newly-served
	// pixels follow key rotation while the tracker still validates the overlap
	// set. Falls back to adserving.DefaultSigningKey until a secret is configured.
	adSecrets := secrets.Start(context.Background(), cfg, clk, log, constants.ServiceAdServer)
	lc.OnShutdown("adserver-secrets-cache", func(_ context.Context) error { adSecrets.Stop(); return nil })
	adSecrets.WatchActive(context.Background(), secrets.PurposeHMACTracker, 30*time.Second, adserving.SetActiveSigningKey)

	port := keys.AdServer.Port.Get(cfg)
	trackerURL := keys.AdServer.TrackerURL.Get(cfg)

	otelShutdown := tracing.Init(context.Background(), tracing.Config{
		ServiceName:    constants.ServiceAdServer,
		ServiceVersion: keys.Otel.ServiceVersion.Get(cfg),
		Endpoint:       keys.Otel.Endpoint.Get(cfg),
		SampleRatio:    keys.Otel.SampleRatio.Get(cfg),
		Log:            log,
	})
	lc.OnShutdown("otel", func(ctx context.Context) error { return otelShutdown(ctx) })

	// Redis freq cap + per-campaign cap warm cache. The counter lives in
	// Redis; the limit/window comes from the campaign's advertiser-configured
	// cap when present, else the platform-default live-config knobs.
	l2 := connectRedis(cfg, log)
	freqCap := NewFreqCap(l2, log)
	freqCapCache := startFreqCapCache(cfg, clk, log)
	if freqCapCache != nil {
		lc.OnShutdown("freq-cap-cache", func(_ context.Context) error { freqCapCache.Stop(); return nil })
	}

	// Object store for large creative bodies
	objStore := objects.Connect(cfg, "/tmp/adtech-creatives", log)
	bucket := keys.S3.Bucket.Get(cfg)

	// Warm cache of creative metadata from Postgres
	metaCache := startCreativeMetaCache(cfg, clk, log)
	if metaCache != nil {
		lc.OnShutdown("creative-meta", func(_ context.Context) error { metaCache.Stop(); return nil })
	}

	// Ad-server event publisher — handles render_failed (resolver miss)
	// and freq_cap_blocked (pre-serve suppression). NATS-only, no HTTP
	// fallback: these are observability signals, not billing-correctness
	// triggers, so a NATS outage just means the event is lost rather
	// than triggering a standalone path. Nil bus = single-process tests;
	// publisher becomes a no-op.
	adserverPub := events.NewPublisher(connectNATS(cfg, log), log)

	// Readiness: L2 connection alive + creative cache has loaded at least once.
	hlth.AddReadinessCheck("l2", func(ctx context.Context) error {
		return l2.Ping(ctx)
	})
	hlth.AddReadinessCheck("creative-cache", func(_ context.Context) error {
		ts, err := metaCache.LastLoaded()
		if err != nil {
			return err
		}
		if ts.IsZero() {
			return errors.New("creative cache not yet loaded")
		}
		return nil
	})

	resolver := NewCreativeResolver(metaCache, objStore, bucket, knobs.CreativeTTL.Value, clk, log)

	// Bandit warm-start: use whatever creatives loaded into the metadata cache
	creativeIDs := resolver.ListIDs()
	bandit := optimise.NewBandit(creativeIDs)
	// Seed the bandit's arms from reporting's historical per-creative CTR so a
	// restart doesn't reset every creative to a uniform prior (ADR 0003 part C).
	// Async + fail-open — never blocks boot, never touches the serve hot path.
	go warmStartBandit(cfg, bandit, log)

	metrics := middleware.NewMetrics(constants.ServiceAdServer)

	mux := http.NewServeMux()
	mux.Handle(routes.Healthz, hlth.LivenessHandler())
	mux.Handle(routes.Readyz, hlth.ReadinessHandler())
	mux.Handle(routes.Metrics, metrics.Handler())
	mux.HandleFunc(routes.AdServe, serveHandler(log, resolver, freqCap, freqCapCache, knobs.FreqCapLimit.Value, knobs.FreqCapWindow.Value, trackerURL, adserverPub, knobs.URLTTL.Value))

	mux.HandleFunc(routes.AdBandit, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"stats":   bandit.Stats(),
			"weights": bandit.Weights(),
		})
	})

	mux.HandleFunc(routes.AdCreatives, func(w http.ResponseWriter, r *http.Request) {
		// Returns the warm-cache snapshot projected to the fields the
		// pub sim's Creatives panel renders. Format=full keeps the
		// previous shape (id list only) for backwards compat with
		// the bandit warm-start path. Default = rich projection
		// (id, name, format, dimensions, review_status, has_html).
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		if r.URL.Query().Get("format") == "ids" {
			json.NewEncoder(w).Encode(resolver.ListIDs())
			return
		}
		rows := resolver.MetaCache().All()
		out := make([]map[string]any, 0, len(rows))
		for _, c := range rows {
			out = append(out, map[string]any{
				"id":            c.ID,
				"name":          c.Name,
				"format":        c.Format,
				"width":         c.Width,
				"height":        c.Height,
				"review_status": c.ReviewStatus,
				"has_html":      c.HTMLContent != "",
				"asset_url":     c.AssetURL,
			})
		}
		json.NewEncoder(w).Encode(out)
	})

	if keys.Debug.EndpointsEnabled.Get(cfg) {
		mux.HandleFunc(routes.DebugCacheRefresh, warm.RefreshHandler(metaCache, freqCapCache))
	}

	// Per-IP rate limit on the public ad-serving endpoints (ratelimit_rps=0 →
	// disabled: a nil limiter that passes through). Innermost so CORS preflight
	// is handled before a request is counted.
	adRL := middleware.NewRateLimiter(keys.AdServer.RateLimitRPS.Get(cfg), keys.AdServer.RateLimitBurst.Get(cfg), log)
	handler := tracing.HTTPMiddleware(constants.ServiceAdServer)(metrics.Wrap(middleware.CORS(adRL.Wrap(mux))))
	server := &http.Server{Addr: ":" + port, Handler: handler, ReadTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second}

	log.Info("adserver starting",
		"port", port,
		"creatives", len(creativeIDs),
		"tracker", trackerURL,
		"bucket", bucket,
	)
	lifecycle.ServeHTTP(lc, server, log, 30*time.Second)
}

// startCreativeMetaCache wires the warm cache to Postgres, falling back to
// an empty in-memory loader when Postgres is unavailable so the service
// boots in offline dev environments (it serves a default creative in that case).
func startCreativeMetaCache(cfg *config.Config, clk clock.Clock, log *slog.Logger) *warm.Cache[models.Creative] {
	pollInterval := firstNonZeroDuration(
		cfg.GetDuration(keys.AdServer.WarmCreativesPollInterval.Key(), 0),
		keys.CacheWarm.PollInterval.Get(cfg),
	)
	loader := pickCreativeLoader(cfg, log)
	bus := connectNATS(cfg, log)
	c := warm.New(warm.Config[models.Creative]{
		Name:              "creatives",
		Loader:            loader,
		Clock:             clk,
		Bus:               bus,
		InvalidateSubject: events.SubjectCacheInvalidateCreatives,
		PollInterval:      pollInterval,
		Log:               log,
	})
	if err := c.Start(context.Background()); err != nil {
		log.Error("creative meta cache initial load failed", "error", err)
	}
	return c
}

// startFreqCapCache warm-caches per-campaign frequency caps (advertiser-
// configured limit/window from targeting_rules.frequency_caps). Invalidated by
// the campaigns subject — a campaign PATCH that edits the cap re-publishes it.
// Nil-tolerant: if Postgres is unreachable the serve path falls back to the
// platform-default cap for every campaign.
func startFreqCapCache(cfg *config.Config, clk clock.Clock, log *slog.Logger) *warm.Cache[models.FreqCapRule] {
	pollInterval := firstNonZeroDuration(
		cfg.GetDuration(keys.AdServer.WarmFreqCapsPollInterval.Key(), 0),
		keys.CacheWarm.PollInterval.Get(cfg),
	)
	dbURL := cfg.Get(keys.Database.URL.Key(), "")
	loader := &warm.RetryingLoader[models.FreqCapRule]{
		Log:   log,
		KeyFn: func(r models.FreqCapRule) string { return r.CampaignID },
		Construct: func() (warm.Loader[models.FreqCapRule], error) {
			if dbURL == "" {
				return nil, fmt.Errorf("database.url not set")
			}
			store, err := postgres.New(postgres.Config{PrimaryURL: dbURL, MaxOpenConns: 5, MaxIdleConns: 2, ConnMaxLifetime: 5 * time.Minute})
			if err != nil {
				return nil, fmt.Errorf("postgres connect: %w", err)
			}
			return &postgres.FreqCapLoader{Store: store}, nil
		},
	}
	c := warm.New(warm.Config[models.FreqCapRule]{
		Name:              "freq_caps",
		Loader:            loader,
		Clock:             clk,
		Bus:               connectNATS(cfg, log),
		InvalidateSubject: events.SubjectCacheInvalidateCampaigns,
		PollInterval:      pollInterval,
		Log:               log,
	})
	if err := c.Start(context.Background()); err != nil {
		log.Error("freq cap cache initial load failed", "error", err)
	}
	return c
}

// pickCreativeLoader returns a self-healing warm.Loader. Lazy-opens
// Postgres on first LoadAll so an adserver that boots before Postgres
// is reachable picks up creatives automatically on the next poll.
func pickCreativeLoader(cfg *config.Config, log *slog.Logger) warm.Loader[models.Creative] {
	dbURL := cfg.Get(keys.Database.URL.Key(), "")
	return &warm.RetryingLoader[models.Creative]{
		Log:   log,
		KeyFn: func(c models.Creative) string { return c.ID },
		Construct: func() (warm.Loader[models.Creative], error) {
			if dbURL == "" {
				return nil, fmt.Errorf("database.url not set")
			}
			store, err := postgres.New(postgres.Config{PrimaryURL: dbURL, MaxOpenConns: 5, MaxIdleConns: 2, ConnMaxLifetime: 5 * time.Minute})
			if err != nil {
				return nil, fmt.Errorf("postgres connect: %w", err)
			}
			return &postgres.CreativeLoader{Store: store}, nil
		},
	}
}

// connectRedis returns a real Redis L2 cache if reachable, else MemoryL2.
func connectRedis(cfg *config.Config, log *slog.Logger) cache.L2Cache {
	addr := keys.Redis.URL.Get(cfg)
	pwd := keys.Redis.Password.Get(cfg)
	db := keys.Redis.DB.Get(cfg)
	// Self-healing: a failed boot dial no longer latches MemoryL2 forever —
	// the wrapper serves fail-open from memory and swaps to Redis when the
	// background retry lands (pkg/cache/selfheal.go).
	return cache.NewSelfHealingL2(func(ctx context.Context) (cache.L2Cache, error) {
		return cacheredis.New(ctx, cacheredis.Config{Addr: addr, Password: pwd, DB: db})
	}, 10*time.Second, addr, log)
}

func connectNATS(cfg *config.Config, log *slog.Logger) events.EventBus {
	url := cfg.Get(keys.AdServer.NATSURL.Key(), keys.Exchange.NATSURL.Get(cfg))
	bus, err := natsbus.New(url, constants.ServiceAdServer, log)
	if err != nil {
		log.Warn("nats unavailable, creative cache will poll only", "error", err)
		return nil
	}
	return bus
}

func serveHandler(log *slog.Logger, resolver *CreativeResolver, freqCap *FreqCap, freqCapCache *warm.Cache[models.FreqCapRule], defaultLimitFn func() int, defaultWindowFn func() time.Duration, trackerURL string, adserverPub *events.Publisher, urlTTLFn func() time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req models.ServeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}

		ctx := logger.WithTraceID(r.Context(), req.TraceID)
		reqLog := logger.WithContext(log, ctx)

		// Resolve the cap: the campaign's advertiser-configured limit/window
		// when present in the warm cache, else the platform-default knobs.
		capLimit, capWindow := defaultLimitFn(), defaultWindowFn()
		if freqCapCache != nil {
			if rule, ok := freqCapCache.ByID(req.CampaignID); ok && rule.Limit > 0 {
				capLimit, capWindow = rule.Limit, rule.Window
			}
		}
		// Per-user cap, then per-household when the SSP derived an hh: id —
		// co-viewing devices (CTV + phones on one IP) share the household
		// counter, which is how CTV capping works when user ids differ or are
		// absent. The hh: prefix keeps the two Redis keyspaces disjoint. A
		// user-cap increment that the household cap then blocks leaves the
		// user counter one high for the window — same increment-then-no-render
		// drift the cap already has on creative-resolve failure; accepted.
		allowed := freqCap.AllowAndRecord(ctx, req.UserID, req.CampaignID, capLimit, capWindow)
		if allowed && req.HouseholdID != "" {
			allowed = freqCap.AllowAndRecord(ctx, req.HouseholdID, req.CampaignID, capLimit, capWindow)
		}
		if !allowed {
			reqLog.Info("ad blocked by freq cap",
				"user", req.UserID,
				"household", req.HouseholdID,
				"campaign", req.CampaignID,
				"placement_id", req.PlacementID,
				"publisher_id", req.PublisherID)
			go adserverPub.AdserverFreqCapBlocked(context.WithoutCancel(ctx),
				events.AdserverFreqCapBlockedEvent{
					TraceID:     req.TraceID,
					UserID:      req.UserID,
					CampaignID:  req.CampaignID,
					PlacementID: req.PlacementID,
					PublisherID: req.PublisherID,
					Timestamp:   time.Now().UTC(),
				})
			http.Error(w, "frequency cap exceeded", http.StatusTooManyRequests)
			return
		}

		creative, ok := resolver.Get(ctx, req.CreativeID)
		if !ok {
			reqLog.Warn("unknown creative, falling back to default HTML",
				"requested_creative_id", req.CreativeID,
				"campaign_id", req.CampaignID,
				"placement_id", req.PlacementID)
			go adserverPub.AdserverRenderFailed(context.WithoutCancel(ctx),
				events.AdserverRenderFailedEvent{
					TraceID:     req.TraceID,
					CampaignID:  req.CampaignID,
					CreativeID:  req.CreativeID,
					PlacementID: req.PlacementID,
					PublisherID: req.PublisherID,
					Reason:      "unknown_creative",
					Detail:      req.CreativeID,
					Timestamp:   time.Now().UTC(),
				})
			creative = AdCreative{
				ID:   req.CreativeID,
				Name: "Dynamic Creative",
				HTML: defaultCreativeHTML,
			}
		}

		macroCtx := adserving.MacroContext{
			AuctionID:    req.TraceID,
			AuctionPrice: req.ClearingPrice,
			Currency:     req.Currency,
			CampaignID:   req.CampaignID,
			CreativeID:   req.CreativeID,
			PlacementID:  req.PlacementID,
			PublisherID:  req.PublisherID,
			AdvertiserID: req.AdvertiserID,
			IOId:         req.IOId,
			DealID:       req.DealID,
			BidModel:     req.BidModel,
			SiteDomain:   req.SiteDomain,
			Width:        req.Width,
			Height:       req.Height,
			Geo:          req.Geo,
			Device:       req.Device,
			UserID:       req.BehaviourUserID, // consent-gated upstream (empty = no personalisation consent)
			TrackerURL:   trackerURL,
			LandingURL:   creative.LandingURL,
			URLTTL:       urlTTLFn(),
		}

		renderedHTML := adserving.SubstituteMacros(creative.HTML, macroCtx)
		impressionURL := adserving.BuildImpressionURL(macroCtx)
		clickURL := adserving.BuildClickURL(macroCtx)
		viewabilityURL := adserving.BuildViewabilityURL(macroCtx)

		resp := models.ServeResponse{
			HTML:           renderedHTML,
			ImpressionURL:  impressionURL,
			ClickURL:       clickURL,
			ViewabilityURL: viewabilityURL,
			TraceID:        req.TraceID,
			CreativeID:     req.CreativeID,
			CampaignID:     req.CampaignID,
			PlacementID:    req.PlacementID,
			PublisherID:    req.PublisherID,
			AdvertiserID:   req.AdvertiserID,
			ClearingPrice:  req.ClearingPrice,
			Currency:       req.Currency,
			Width:          req.Width,
			Height:         req.Height,
		}

		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		json.NewEncoder(w).Encode(resp)

		reqLog.Info("ad served",
			"creative", req.CreativeID,
			"campaign", req.CampaignID,
			"placement", req.PlacementID,
			"clearing_price", req.ClearingPrice,
		)
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

const defaultCreativeHTML = `<div style="width:${WIDTH}px;height:${HEIGHT}px;background:#f5f5f5;border:1px solid #ddd;display:flex;flex-direction:column;align-items:center;justify-content:center;font-family:sans-serif;">
  <h3 style="margin:0 0 8px;color:#333;">Advertisement</h3>
  <p style="margin:0;color:#666;font-size:12px;">Campaign: ${CAMPAIGN_ID}</p>
  <p style="margin:0;color:#666;font-size:12px;">Creative: ${CREATIVE_ID}</p>
</div>`
