// cmd/dsp is the Demand-Side Platform service.
// Manages campaigns, evaluates bid requests, submits bids.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand"
	"net/http"
	"strconv"
	"time"

	audstore "github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store"
	audcached "github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store/cached"
	audiencepg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store/postgres"
	audpreload "github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store/preload"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/bidshading"
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
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/pacing"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/targeting"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
	_ "github.com/lib/pq"
)

func main() {
	clk := clock.Real{}
	log := logger.New(constants.ServiceDSP)
	sc := config.Setup(constants.ServiceDSP, log)
	cfg := sc.Cfg
	_ = sc
	hlth := health.New()
	lc := lifecycle.New(log)

	port := cfg.Get("dsp.port", routes.PortDSP)
	profile := cfg.Get("dsp.profile", "internal")

	// OpenTelemetry: traces inbound from exchange via HTTP middleware,
	// becomes a child of the exchange.auction span automatically.
	otelShutdown := tracing.Init(context.Background(), tracing.Config{
		ServiceName:    constants.ServiceDSP,
		ServiceVersion: cfg.Get("otel.service_version", "dev"),
		Endpoint:       cfg.Get("otel.endpoint", "localhost:4318"),
		SampleRatio:    cfg.GetFloat("otel.sample_ratio", 1.0),
		Log:            log,
	})
	lc.OnShutdown("otel", func(ctx context.Context) error { return otelShutdown(ctx) })

	// Publish this service's schema to config_schema so the gateway's
	// config-manager UI sees these keys. nil-tolerant on DB failures —
	// the in-process registry still updates so local Validate calls work.
	config.PublishSchemaWithURL(cfg.Get("database.url", ""), constants.ServiceDSP, dspSchema, log)

	// DSP identity: look up this pod's own row in the `dsps` table by name.
	// The row is the source of truth for noise_pct + no_bid_rate (replaces
	// the old YAML+config triple). YAML profile becomes a back-compat
	// fallback only — if Postgres is unreachable or the row doesn't exist
	// yet (fresh dev environment), we fall back to YAML defaults so the
	// pod still boots.
	dspProfile, dspRow := loadDSPIdentity(cfg, profile, log)
	noisePct := float64(dspRow.NoisePct)
	noBidRate := dspRow.NoBidRate
	isCompetitor := dspRow.IsCompetitor()

	// Redis budget tracker.
	l2 := connectRedis(cfg, log)
	budget := NewBudgetTracker(l2, cfg.GetDuration("dsp.budget_reset_interval", 24*time.Hour), log)
	shadingTracker := bidshading.NewTracker()

	// NATS bus for warm-cache invalidate subscription. nil-tolerant — the
	// warm cache degrades to poll-only mode if NATS is unreachable.
	bus := connectNATS(cfg, log)

	// Postgres + warm cache for campaigns. dspID is the filter — only
	// campaigns under accounts where accounts.dsp_id matches this pod's
	// DSP make it into the cache. accountIDs allowlist is kept as a
	// fallback for the YAML-only boot path (Postgres unreachable).
	accountIDs := derivedAccountUUIDs(dspProfile)
	campaignCache := startCampaignCache(cfg, clk, log, dspProfile, bus, dspRow.ID, accountIDs)
	if campaignCache != nil {
		lc.OnShutdown("campaign-cache", func(_ context.Context) error { campaignCache.Stop(); return nil })
	}

	// Path B: DSP-private audience segments. Looked up per bid request and
	// unioned with SSP-stamped segments before targeting evaluation. Nil
	// store = no enrichment, DSP keeps bidding on whatever the SSP sent.
	audienceStore, audienceStop := openAudienceStore(cfg, l2, log)
	lc.OnShutdown("audience-store", func(_ context.Context) error { audienceStop(); return nil })

	// Readiness checks: only report ready when the L2 connection responds
	// and the campaign cache has completed at least one successful load.
	// Tilt and K8s use this to decide when to route traffic / show green.
	hlth.AddReadinessCheck("l2", func(ctx context.Context) error {
		return l2.Ping(ctx)
	})
	hlth.AddReadinessCheck("campaign-cache", func(_ context.Context) error {
		ts, err := campaignCache.LastLoaded()
		if err != nil {
			return err
		}
		if ts.IsZero() {
			return errors.New("campaign cache not yet loaded")
		}
		return nil
	})

	metrics := middleware.NewMetrics(constants.ServiceDSP)

	mux := http.NewServeMux()
	mux.Handle(routes.Healthz, hlth.LivenessHandler())
	mux.Handle(routes.Readyz, hlth.ReadinessHandler())
	mux.Handle(routes.Metrics, metrics.Handler())
	mux.HandleFunc(routes.OpenRTBBid, bidHandler(log, clk, campaignCache, audienceStore, budget, isCompetitor, noisePct, noBidRate))

	mux.HandleFunc(routes.OpenRTBWin, winHandler(log, budget, shadingTracker))
	mux.HandleFunc(routes.OpenRTBLoss, lossHandler(log, shadingTracker))

	mux.HandleFunc(routes.DSPShading, func(w http.ResponseWriter, r *http.Request) {
		placements := shadingTracker.AllPlacements()
		result := make(map[string]bidshading.PlacementStats)
		for _, pid := range placements {
			result[pid] = shadingTracker.Stats(pid)
		}
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		json.NewEncoder(w).Encode(result)
	})

	// Campaign management (read + create + edit + delete). These are real
	// platform endpoints, not debug surface — the publisher simulator is
	// just the first consumer; the eventual Gateway admin UI / partner
	// API will use the same routes. Always on regardless of debug flags.
	//
	// TODO (production prerequisite): wire an auth middleware that requires
	// a bearer token / API key on mutating methods (POST/PATCH/DELETE),
	// resolves the caller's identity, and enforces per-account scoping
	// (caller can only mutate campaigns under accounts they have access to).
	// Today every request is implicitly trusted — fine for local dev where
	// the network is closed, but the endpoints MUST NOT ship to a public
	// surface without auth. See PLAN.md "External DSP Partners → Auth-aware
	// HTTP client" for the partner-facing equivalent; the same `pkg/middleware`
	// auth primitives should cover both inbound (this surface) and outbound
	// (exchange→partner) flows.
	mgmtDB := openManagementDB(cfg, log)
	if mgmtDB != nil {
		lc.OnShutdown("mgmt-db", func(_ context.Context) error { return mgmtDB.Close() })
	}
	mux.HandleFunc(routes.DSPCampaigns+"/", campaignByIDHandler(mgmtDB, bus, accountIDs, log))
	mux.HandleFunc(routes.DSPCampaigns, campaignsCollectionHandler(campaignCache, mgmtDB, bus, dspRow.ID, dspRow.Name, budget, log))

	// Cache refresh stays debug-gated — it's purely a dev/test helper for
	// forcing a synchronous reload, not a customer-facing operation.
	if cfg.GetBool("debug.endpoints_enabled", true) {
		mux.HandleFunc(routes.DebugCacheRefresh, warm.RefreshHandler(campaignCache))
	}

	handler := tracing.HTTPMiddleware(constants.ServiceDSP)(metrics.Wrap(middleware.CORS(mux)))
	server := &http.Server{Addr: ":" + port, Handler: handler, ReadTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second}

	log.Info("dsp starting",
		"port", port,
		"profile", profile,
		"campaigns", campaignCache.Len(),
		"is_competitor", isCompetitor,
	)
	lifecycle.ServeHTTP(lc, server, log, 30*time.Second)
}

// loadDSPIdentity returns this pod's DSP identity. Prefers the Postgres
// dsps row (the source of truth post-migration 022); falls back to the YAML
// profile when the DB is unreachable so dev environments still boot.
//
// dspProfile is still returned alongside because the YAML carries seed
// campaign definitions used by the in-memory loader fallback below. Once
// Postgres-only mode is enforced (after the rollup of the YAML fallback
// loaders), this can collapse to just the DSPRow.
func loadDSPIdentity(cfg *config.Config, profileName string, log *slog.Logger) (*DSPProfile, *postgres.DSPRow) {
	dspProfile, err := FindProfile(profileName, log)
	if err != nil {
		log.Warn("profile YAML not found, defaults will be used", "profile", profileName, "error", err)
		dspProfile = &DSPProfile{Name: profileName, NoisePct: 30, NoBidRate: 0.20}
	}

	dbURL := cfg.Get("database.url", "")
	if dbURL == "" {
		return dspProfile, fallbackDSPRow(dspProfile)
	}
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Warn("dsp identity db open failed, using YAML defaults", "error", err)
		return dspProfile, fallbackDSPRow(dspProfile)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	row, err := postgres.DSPByName(ctx, db, profileName)
	if err != nil {
		log.Warn("dsps row lookup failed, using YAML defaults", "profile", profileName, "error", err)
		return dspProfile, fallbackDSPRow(dspProfile)
	}
	log.Info("dsp identity loaded from postgres", "name", row.Name, "id", row.ID, "type", row.ProfileType, "noise_pct", row.NoisePct, "no_bid_rate", row.NoBidRate)
	return dspProfile, row
}

// fallbackDSPRow synthesises a DSPRow from the YAML profile when the DB
// is unavailable. The ID will be empty — the CampaignLoader treats that as
// "no dsp_id filter" and falls back to the YAML-derived accountIDs allowlist.
func fallbackDSPRow(p *DSPProfile) *postgres.DSPRow {
	row := &postgres.DSPRow{
		Name:      p.Name,
		NoisePct:  int(p.NoisePct),
		NoBidRate: p.NoBidRate,
		Status:    "active",
	}
	if row.NoisePct > 0 || row.NoBidRate > 0 {
		row.ProfileType = "competitor"
	} else {
		row.ProfileType = "internal"
	}
	return row
}

// openAudienceStore returns a read-side Lookup for DSP-private segments.
// When Redis and Postgres are both reachable, the warm preloader is used —
// a background worker pumps the whole audience_segment_members table into
// Redis on a schedule, so the bid path never queries Postgres directly.
// Falls back to a lazy postgres+redis cache if the preloader can't start,
// or to postgres-direct if no L2 cache is present, or nil if no DB at all.
//
// Returns the Lookup + a Stop function the caller registers on shutdown.
func openAudienceStore(cfg *config.Config, l2 cache.L2Cache, log *slog.Logger) (audstore.Lookup, func()) {
	dbURL := cfg.Get("database.url", "")
	if dbURL == "" {
		log.Warn("database.url not set, dsp audience store disabled")
		return nil, func() {}
	}
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Warn("dsp audience store open failed", "error", err)
		return nil, func() {}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		log.Warn("dsp audience store ping failed", "error", err)
		_ = db.Close()
		return nil, func() {}
	}
	if l2 == nil {
		log.Info("dsp audience store connected (postgres-direct, no L2 cache)")
		return audiencepg.New(db), func() { _ = db.Close() }
	}
	interval := cfg.GetDuration("audience.preload_interval", 30*time.Second)
	ttl := cfg.GetDuration("audience.cache_ttl", 90*time.Second)
	pre := audpreload.New(audpreload.Config{DB: db, L2: l2, Interval: interval, TTL: ttl, Log: log})
	if err := pre.Start(context.Background()); err != nil {
		log.Warn("audience preloader start failed, falling back to lazy cache", "error", err)
		return audcached.New(audiencepg.New(db), l2, ttl, log), func() { _ = db.Close() }
	}
	log.Info("dsp audience store connected (redis warm preload)", "interval", interval, "ttl", ttl)
	return pre, func() { pre.Stop(); _ = db.Close() }
}

// startCampaignCache wires the warm cache to Postgres, or to a YAML-derived
// memory loader if Postgres is unreachable. Either way the bid handler reads
// from a *warm.Cache[models.Campaign]. Bus is passed in (rather than dialled
// here) so other subscribers in the service can share one NATS connection.
func startCampaignCache(cfg *config.Config, clk clock.Clock, log *slog.Logger, profile *DSPProfile, bus events.EventBus, dspID string, accountIDs []string) *warm.Cache[models.Campaign] {
	pollInterval := firstNonZeroDuration(
		cfg.GetDuration("cache.warm.campaigns.poll_interval", 0),
		cfg.GetDuration("cache.warm.poll_interval", 30*time.Second),
	)

	loader := pickCampaignLoader(cfg, log, profile, dspID, accountIDs)
	c := warm.New(warm.Config[models.Campaign]{
		Name:              "campaigns",
		Loader:            loader,
		Clock:             clk,
		Bus:               bus,
		InvalidateSubject: events.SubjectCacheInvalidateCampaigns,
		PollInterval:      pollInterval,
		Log:               log,
	})
	if err := c.Start(context.Background()); err != nil {
		log.Error("campaign cache initial load failed", "error", err)
	}
	return c
}

// pickCampaignLoader returns the Postgres loader, or a YAML-derived
// in-memory loader if Postgres can't be reached. The interface is the same
// either way so the warm cache doesn't care.
func pickCampaignLoader(cfg *config.Config, log *slog.Logger, profile *DSPProfile, dspID string, accountIDs []string) warm.Loader[models.Campaign] {
	dbURL := cfg.Get("database.url", "")
	if dbURL == "" {
		log.Warn("database.url not set, using YAML in-memory campaign loader")
		return &yamlCampaignLoader{profile: profile}
	}
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Warn("postgres open failed, using YAML loader", "error", err)
		return &yamlCampaignLoader{profile: profile}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		log.Warn("postgres ping failed, using YAML loader", "error", err)
		_ = db.Close()
		return &yamlCampaignLoader{profile: profile}
	}
	log.Info("postgres connected for campaign loader", "url_masked", maskedURL(dbURL), "dsp_id", dspID)
	store, _ := postgres.New(postgres.Config{PrimaryURL: dbURL, MaxOpenConns: 5, MaxIdleConns: 2, ConnMaxLifetime: 5 * time.Minute})
	// DSPID is the primary filter (joins accounts.dsp_id). AccountIDs is
	// kept as a YAML-derived fallback for environments where the dsps row
	// doesn't exist yet or migration 022 hasn't run.
	return &postgres.CampaignLoader{Store: store, DSPID: dspID, AccountIDs: accountIDs}
}

// connectNATS returns a JetStream-backed bus if reachable, else nil
// (warm cache falls back to poll-only mode).
func connectNATS(cfg *config.Config, log *slog.Logger) events.EventBus {
	url := cfg.Get("dsp.nats_url", cfg.Get("exchange.nats_url", routes.DefaultNATSURL))
	bus, err := natsbus.New(url, constants.ServiceDSP, log)
	if err != nil {
		log.Warn("nats unavailable, warm cache will poll only", "error", err)
		return nil
	}
	return bus
}

// derivedAccountUUIDs converts the profile YAML's friendly account IDs to
// the UUIDs the seeder writes into Postgres. Same derivation in both places
// guarantees the WHERE clause matches the inserted rows.
func derivedAccountUUIDs(profile *DSPProfile) []string {
	seen := map[string]struct{}{}
	var ids []string
	for _, c := range profile.Campaigns {
		if c.AccountID == "" {
			continue
		}
		if _, ok := seen[c.AccountID]; ok {
			continue
		}
		seen[c.AccountID] = struct{}{}
		ids = append(ids, idgen.Derive("account", c.AccountID))
	}
	return ids
}

// yamlCampaignLoader is the no-Postgres fallback. Returns the YAML campaigns
// converted to models.Campaign with deterministic UUIDs (same as the seed).
type yamlCampaignLoader struct{ profile *DSPProfile }

func (l *yamlCampaignLoader) LoadAll(_ context.Context) ([]models.Campaign, error) {
	out := make([]models.Campaign, 0, len(l.profile.Campaigns))
	for _, cc := range l.profile.Campaigns {
		c := models.Campaign{
			ID:             idgen.Derive("line_item", cc.ID),
			AccountID:      idgen.Derive("account", cc.AccountID),
			AdvertiserID:   idgen.Derive("account", defaultStr(cc.AdvertiserID, cc.AccountID)),
			IOId:           idgen.Derive("io", cc.IOId),
			Name:           cc.Name,
			CreativeID:     idgen.Derive("creative", cc.CreativeID),
			CreativeDomain: cc.CreativeDomain,
			BaseBid:        cc.BaseBid,
			Currency:       cc.Currency,
			DailyBudget:    cc.DailyBudget,
			TotalBudget:    cc.TotalBudget,
			BidModel:       cc.BidModel,
			PacingMode:     cc.PacingMode,
			Status:         cc.Status,
		}
		if cc.Targeting != nil {
			c.Targeting = targeting.Rules{
				Include: targeting.TargetingSet{
					Geo: cc.Targeting.Include.Geo, Device: cc.Targeting.Include.Device,
					OS: cc.Targeting.Include.OS, Segments: cc.Targeting.Include.Segments,
					Domains: cc.Targeting.Include.Domains, Categories: cc.Targeting.Include.Categories,
				},
				Exclude: targeting.TargetingSet{
					Geo: cc.Targeting.Exclude.Geo, Device: cc.Targeting.Exclude.Device,
					Domains: cc.Targeting.Exclude.Domains, Categories: cc.Targeting.Exclude.Categories,
				},
			}
		}
		if cc.Modifiers != nil {
			c.Modifiers = targeting.Modifiers{Device: cc.Modifiers.Device, GeoCountry: cc.Modifiers.GeoCountry}
		}
		out = append(out, c)
	}
	return out, nil
}

func (l *yamlCampaignLoader) KeyOf(c models.Campaign) string { return c.ID }

// connectRedis returns a real Redis L2 cache if reachable, falling back
// to MemoryL2 with a warning so dev environments without Redis still boot.
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

func bidHandler(log *slog.Logger, clk clock.Clock, campaigns *warm.Cache[models.Campaign], audienceStore audstore.Lookup, budget *BudgetTracker, isCompetitor bool, noisePct, noBidRate float64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var bidReq openrtb.BidRequest
		if err := json.NewDecoder(r.Body).Decode(&bidReq); err != nil {
			http.Error(w, "invalid bid request", http.StatusBadRequest)
			return
		}

		ctx := logger.WithTraceID(r.Context(), bidReq.ID)
		reqLog := logger.WithContext(log, ctx)

		tReq := targeting.Request{
			Device:        deviceTypeStr(bidReq.Device),
			InventoryType: inventoryType(bidReq),
		}
		if bidReq.Device != nil && bidReq.Device.Geo != nil {
			tReq.Geo = bidReq.Device.Geo.Country
		}
		if bidReq.Site != nil {
			tReq.Domain = bidReq.Site.Domain
			tReq.Categories = bidReq.Site.Cat
			if len(tReq.Categories) == 0 {
				classifier := targeting.NewClassifier()
				tReq.Categories = classifier.Classify(bidReq.Site.Domain, bidReq.Site.Page, nil, nil)
			}
		}
		if bidReq.User != nil && bidReq.User.Ext != nil {
			tReq.Segments = bidReq.User.Ext.Segments
		}

		// Dev-mode artificial latency: a publisher-simulator request can set
		// X-Dev-Delay-Ms on the inbound bid request (propagated by the exchange
		// from the simulator's selection) to make this DSP sleep before
		// responding. Used to demonstrate the fan-out timeout / early-finish
		// behavior in the UI. Gated by debug.endpoints_enabled so prod can't
		// be slowed by a forged header.
		if delayStr := r.Header.Get("X-Dev-Delay-Ms"); delayStr != "" {
			if ms, err := strconv.Atoi(delayStr); err == nil && ms > 0 && ms < 5000 {
				time.Sleep(time.Duration(ms) * time.Millisecond)
			}
		}
		// Path B: union the SSP-stamped (public) segments with the DSP's
		// own private segments — CRM uploads, retargeting pixels, lookalikes
		// that other DSPs never get to see. Targeting evaluation can then
		// match a "luxury_watch_intent" campaign without the SSP ever
		// knowing that label exists.
		//
		// Tight deadline (25ms) on this lookup: it's a "nice to have" for
		// targeting refinement, not load-bearing for the bid. Under
		// concurrent load on the local stack, the shared Postgres pool can
		// queue queries past the inherited 100ms bid_timeout — letting the
		// inherited deadline fire here means the WHOLE bid is canceled
		// (5xx + no-bid). With our own 25ms cap, slow lookups degrade
		// gracefully: bid proceeds without private segments instead of
		// failing entirely.
		if audienceStore != nil && bidReq.User != nil && bidReq.User.ID != "" {
			lookupCtx, cancel := context.WithTimeout(r.Context(), 25*time.Millisecond)
			private, err := audienceStore.DSPSegmentsForUser(lookupCtx, bidReq.User.ID)
			cancel()
			if err != nil {
				log.Debug("dsp private segment lookup degraded (bid proceeds without)", "user_id", bidReq.User.ID, "error", err)
			} else if len(private) > 0 {
				tReq.Segments = append(tReq.Segments, private...)
			}
		}

		floor := 0.0
		if len(bidReq.Imp) > 0 {
			floor = bidReq.Imp[0].BidFloor
		}

		all := campaigns.All()
		var bestBid *openrtb.BidObj
		var bestCampaign *models.Campaign
		var bestPrice float64

		for i := range all {
			c := &all[i]
			if c.Status != constants.StatusLive {
				continue
			}

			result := targeting.Evaluate(c.Targeting, tReq)
			if !result.Matched {
				reqLog.Debug("campaign excluded by targeting", "campaign", c.ID, "dimension", result.FailedDimension)
				continue
			}

			pacer := pacing.New(clk, pacing.Config{
				Mode:        pacingMode(c.PacingMode),
				DailyBudget: c.DailyBudget,
				DayStartUTC: clk.Now().Truncate(24 * time.Hour),
			})
			currentSpend := budget.Spend(c.ID)
			if !pacer.ShouldBid(currentSpend) {
				reqLog.Debug("campaign throttled by pacing", "campaign", c.ID, "spend", currentSpend)
				continue
			}
			if currentSpend >= c.DailyBudget {
				reqLog.Debug("campaign daily budget exhausted", "campaign", c.ID)
				continue
			}

			modCtx := targeting.ModifierContext{Device: tReq.Device, GeoCountry: tReq.Geo}
			adjustedBid, _ := targeting.ApplyModifiers(c.BaseBid, c.Modifiers, modCtx)

			if isCompetitor {
				if rand.Float64() < noBidRate {
					reqLog.Debug("competitor random no-bid", "campaign", c.ID)
					continue
				}
				noiseFraction := noisePct / 100.0
				noiseMin := 1.0 - noiseFraction
				noiseRange := noiseFraction * 2.0
				adjustedBid *= noiseMin + rand.Float64()*noiseRange
			}

			if adjustedBid < floor {
				reqLog.Debug("bid below floor", "campaign", c.ID, "bid", adjustedBid, "floor", floor)
				continue
			}

			if adjustedBid > bestPrice {
				bestPrice = adjustedBid
				bestCampaign = c
				bestBid = &openrtb.BidObj{
					ID:      "bid-" + bidReq.ID + "-" + c.ID,
					ImpID:   bidReq.Imp[0].ID,
					Price:   adjustedBid,
					CID:     c.ID,
					CrID:    c.CreativeID,
					ADomain: []string{c.CreativeDomain},
				}
			}
		}

		if bestBid == nil {
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			json.NewEncoder(w).Encode(openrtb.BidResponse{ID: bidReq.ID, NoBid: true})
			reqLog.Info("no bid", "reason", "no eligible campaigns", "candidates", len(all))
			return
		}

		resp := openrtb.BidResponse{
			ID:  bidReq.ID,
			Cur: bestCampaign.Currency,
			SeatBid: []openrtb.SeatBid{{
				Seat: bestCampaign.AccountID,
				Bid:  []openrtb.BidObj{*bestBid},
			}},
		}

		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		json.NewEncoder(w).Encode(resp)

		reqLog.Info("bid submitted",
			"campaign", bestCampaign.ID,
			"campaign_name", bestCampaign.Name,
			"price", bestPrice,
			"creative", bestCampaign.CreativeID,
		)
	}
}

func pacingMode(s string) pacing.Mode {
	switch s {
	case "asap":
		return pacing.ModeASAP
	case "front_loaded":
		return pacing.ModeFrontLoaded
	default:
		return pacing.ModeEven
	}
}

func deviceTypeStr(d *openrtb.Device) string {
	if d == nil {
		return "desktop"
	}
	switch d.DeviceType {
	case 1:
		return "mobile"
	case 2:
		return "desktop"
	case 3:
		return "ctv"
	case 5:
		return "tablet"
	default:
		return "desktop"
	}
}

func inventoryType(req openrtb.BidRequest) string {
	if req.App != nil {
		return "app"
	}
	return "site"
}

// winHandler processes win notifications from the exchange (the OpenRTB
// nurl path). Updates the campaign budget counter and feeds the bid shading
// model. Single source of budget truth for this DSP: internal and external
// DSPs alike are notified through this endpoint. The parallel NATS
// adtech.auction.win event is for non-DSP consumers (reporting analytics,
// future billing ledger), not for re-driving the DSP's own budget.
func winHandler(log *slog.Logger, budget *BudgetTracker, tracker *bidshading.Tracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		bidID := q.Get("bid_id")
		price, _ := strconv.ParseFloat(q.Get("price"), 64)
		placementID := q.Get("placement_id")
		campaignID := q.Get("campaign_id")

		if campaignID != "" {
			budget.Record(campaignID, price)
		}
		if placementID != "" {
			tracker.RecordWin(placementID, price, price)
		}

		log.Info("win notification", "bid_id", bidID, "price", price, "campaign_id", campaignID)
		w.WriteHeader(http.StatusNoContent)
	}
}

// lossHandler processes loss notifications from the exchange.
// Records loss data for the bid shading model.
func lossHandler(log *slog.Logger, tracker *bidshading.Tracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		bidID := q.Get("bid_id")
		reason, _ := strconv.Atoi(q.Get("reason"))
		clearingPrice, _ := strconv.ParseFloat(q.Get("clearing_price"), 64)
		campaignID := q.Get("campaign_id")
		placementID := q.Get("placement_id")

		if placementID != "" {
			tracker.RecordLoss(placementID, clearingPrice, clearingPrice, bidshading.LossReason(reason))
		}

		log.Info("loss notification", "bid_id", bidID, "reason", reason, "clearing_price", clearingPrice, "campaign_id", campaignID)
		w.WriteHeader(http.StatusNoContent)
	}
}

// helpers ---------------------------------------------------------------------

func defaultStr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func firstNonZeroDuration(ds ...time.Duration) time.Duration {
	for _, d := range ds {
		if d > 0 {
			return d
		}
	}
	return 30 * time.Second
}

// maskedURL strips credentials from a Postgres URL for logging.
func maskedURL(url string) string {
	const masked = "postgres://****@"
	if i := indexOf(url, "@"); i > 0 {
		return masked + url[i+1:]
	}
	return url
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}




