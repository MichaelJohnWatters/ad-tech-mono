// cmd/adserver serves ad creatives to end-user browsers.
// Generates tracking URLs with signed parameters via macro substitution.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache"
	cacheredis "github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache/redis"
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
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/optimise"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/fs"
	objs3 "github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/s3"
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
	sc := config.Setup(constants.ServiceAdServer, log)
	config.PublishSchemaWithURL(sc.Cfg.Get("database.url", ""), constants.ServiceAdServer, adserverSchema, log)
	cfg := sc.Cfg
	_ = sc
	hlth := health.New()
	lc := lifecycle.New(log)

	port := cfg.Get("adserver.port", routes.PortAdServer)
	trackerURL := cfg.Get("adserver.tracker_url", routes.DefaultTrackerURL)

	otelShutdown := tracing.Init(context.Background(), tracing.Config{
		ServiceName:    constants.ServiceAdServer,
		ServiceVersion: cfg.Get("otel.service_version", "dev"),
		Endpoint:       cfg.Get("otel.endpoint", "localhost:4318"),
		SampleRatio:    cfg.GetFloat("otel.sample_ratio", 1.0),
		Log:            log,
	})
	lc.OnShutdown("otel", func(ctx context.Context) error { return otelShutdown(ctx) })

	// Redis freq cap
	l2 := connectRedis(cfg, log)
	freqCap := NewFreqCap(
		l2,
		cfg.GetInt("adserver.freq_cap_per_user_per_campaign", 5),
		cfg.GetDuration("adserver.freq_cap_window", 24*time.Hour),
		log,
	)

	// Object store for large creative bodies
	objStore := connectObjects(cfg, log)
	bucket := cfg.Get("s3.bucket", "adtech-creatives")

	// Warm cache of creative metadata from Postgres
	metaCache := startCreativeMetaCache(cfg, clk, log)
	if metaCache != nil {
		lc.OnShutdown("creative-meta", func(_ context.Context) error { metaCache.Stop(); return nil })
	}

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

	resolver := NewCreativeResolver(
		metaCache, objStore, bucket,
		cfg.GetDuration("adserver.default_creative_ttl", 5*time.Minute),
		clk, log,
	)

	// Bandit warm-start: use whatever creatives loaded into the metadata cache
	creativeIDs := resolver.ListIDs()
	bandit := optimise.NewBandit(creativeIDs)

	metrics := middleware.NewMetrics(constants.ServiceAdServer)

	mux := http.NewServeMux()
	mux.Handle(routes.Healthz, hlth.LivenessHandler())
	mux.Handle(routes.Readyz, hlth.ReadinessHandler())
	mux.Handle(routes.Metrics, metrics.Handler())
	mux.HandleFunc(routes.AdServe, serveHandler(log, resolver, freqCap, trackerURL))

	mux.HandleFunc(routes.AdBandit, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"stats":   bandit.Stats(),
			"weights": bandit.Weights(),
		})
	})

	mux.HandleFunc(routes.AdCreatives, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		json.NewEncoder(w).Encode(resolver.ListIDs())
	})

	if cfg.GetBool("debug.endpoints_enabled", true) {
		mux.HandleFunc(routes.DebugCacheRefresh, warm.RefreshHandler(metaCache))
	}

	handler := tracing.HTTPMiddleware(constants.ServiceAdServer)(metrics.Wrap(middleware.CORS(mux)))
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
		cfg.GetDuration("cache.warm.creatives.poll_interval", 0),
		cfg.GetDuration("cache.warm.poll_interval", 30*time.Second),
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

func pickCreativeLoader(cfg *config.Config, log *slog.Logger) warm.Loader[models.Creative] {
	dbURL := cfg.Get("database.url", "")
	if dbURL == "" {
		log.Warn("database.url not set, creatives cache will be empty")
		return emptyCreativeLoader{}
	}
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Warn("postgres open failed", "error", err)
		return emptyCreativeLoader{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		log.Warn("postgres ping failed", "error", err)
		_ = db.Close()
		return emptyCreativeLoader{}
	}
	store, _ := postgres.New(postgres.Config{PrimaryURL: dbURL, MaxOpenConns: 5, MaxIdleConns: 2, ConnMaxLifetime: 5 * time.Minute})
	log.Info("postgres connected for creative loader")
	return &postgres.CreativeLoader{Store: store}
}

type emptyCreativeLoader struct{}

func (emptyCreativeLoader) LoadAll(_ context.Context) ([]models.Creative, error) { return nil, nil }
func (emptyCreativeLoader) KeyOf(c models.Creative) string                       { return c.ID }

// connectRedis returns a real Redis L2 cache if reachable, else MemoryL2.
func connectRedis(cfg *config.Config, log *slog.Logger) cache.L2Cache {
	addr := cfg.Get("redis.url", "localhost:6379")
	pwd := cfg.Get("redis.password", "")
	db := cfg.GetInt("redis.db", 0)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := cacheredis.New(ctx, cacheredis.Config{Addr: addr, Password: pwd, DB: db})
	if err != nil {
		log.Warn("redis unreachable, falling back to in-memory L2", "addr", addr, "error", err)
		return cache.NewMemoryL2()
	}
	log.Info("redis connected", "addr", addr)
	return client
}

// connectObjects returns an S3/Minio store if configured, else a local FS store.
func connectObjects(cfg *config.Config, log *slog.Logger) objects.Store {
	endpoint := cfg.Get("s3.endpoint", "")
	if endpoint == "" {
		root := "/tmp/adtech-creatives"
		log.Warn("s3.endpoint not set, using local filesystem", "root", root)
		store, err := fs.New(root)
		if err != nil {
			log.Error("fs store init failed", "error", err)
		}
		return store
	}
	store, err := objs3.New(objs3.Config{
		Endpoint:  endpoint,
		AccessKey: cfg.Get("s3.access_key", "adtech"),
		SecretKey: cfg.Get("s3.secret_key", "adtech-local-dev"),
		Region:    cfg.Get("s3.region", "us-east-1"),
		UseSSL:    cfg.GetBool("s3.use_ssl", false),
	})
	if err != nil {
		log.Error("s3 init failed, falling back to filesystem", "error", err)
		fsStore, _ := fs.New("/tmp/adtech-creatives")
		return fsStore
	}
	log.Info("s3 connected", "endpoint", endpoint)
	return store
}

func connectNATS(cfg *config.Config, log *slog.Logger) events.EventBus {
	url := cfg.Get("adserver.nats_url", cfg.Get("exchange.nats_url", routes.DefaultNATSURL))
	bus, err := natsbus.New(url, constants.ServiceAdServer, log)
	if err != nil {
		log.Warn("nats unavailable, creative cache will poll only", "error", err)
		return nil
	}
	return bus
}

func serveHandler(log *slog.Logger, resolver *CreativeResolver, freqCap *FreqCap, trackerURL string) http.HandlerFunc {
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

		if !freqCap.AllowAndRecord(ctx, req.UserID, req.CampaignID) {
			reqLog.Info("ad blocked by freq cap", "user", req.UserID, "campaign", req.CampaignID)
			http.Error(w, "frequency cap exceeded", http.StatusTooManyRequests)
			return
		}

		creative, ok := resolver.Get(ctx, req.CreativeID)
		if !ok {
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
			SiteDomain:   req.SiteDomain,
			Width:        req.Width,
			Height:       req.Height,
			TrackerURL:   trackerURL,
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
