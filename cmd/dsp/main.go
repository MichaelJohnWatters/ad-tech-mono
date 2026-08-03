// cmd/dsp is the Demand-Side Platform service.
// Manages campaigns, evaluates bid requests, submits bids.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
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
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events/natsbus"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/health"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/idgen"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/native"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/pacing"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/privacy"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/secrets"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/targeting"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
	_ "github.com/lib/pq"
)

func main() {
	clk := clock.Real{}
	log := logger.New(constants.ServiceDSP)

	// Pre-Setup: read DSP_PROFILE from env (Tilt sets it per-pod) and
	// load the YAML profile to extract per-pod default values for
	// noise_pct and no_bid_rate. These get passed to Setup as seed
	// overrides so the per-pod config row gets the right default value
	// at first boot instead of the schema-wide 0 — without this, every
	// DSP pod would have noise_pct=0 in Postgres regardless of profile,
	// silently making competitor DSPs deterministic. Profile config key
	// is "dsp.profile" with envKey DSP_PROFILE.
	profileFromEnv := os.Getenv("DSP_PROFILE")
	if profileFromEnv == "" {
		profileFromEnv = "internal"
	}
	seedOverrides := dspSeedOverrides(profileFromEnv, log)

	sc := config.Setup(constants.ServiceDSP, keys.DSPSchema(), log,
		config.WithSeedDefaults(seedOverrides))
	cfg := sc.Cfg
	knobs := NewKnobs(sc)
	hlth := health.New()
	lc := lifecycle.New(log)

	port := keys.DSP.Port.Get(cfg)
	profile := knobs.Profile()

	// OpenTelemetry: traces inbound from exchange via HTTP middleware,
	// becomes a child of the exchange.auction span automatically.
	otelShutdown := tracing.Init(context.Background(), tracing.Config{
		ServiceName:    constants.ServiceDSP,
		ServiceVersion: keys.Otel.ServiceVersion.Get(cfg),
		Endpoint:       keys.Otel.Endpoint.Get(cfg),
		SampleRatio:    keys.Otel.SampleRatio.Get(cfg),
		Log:            log,
	})
	lc.OnShutdown("otel", func(ctx context.Context) error { return otelShutdown(ctx) })

	// DSP identity: look up this pod's own row in the `dsps` table by name.
	// The row is the source of truth for noise_pct + no_bid_rate (replaces
	// the old YAML+config triple). YAML profile becomes a back-compat
	// fallback only — if Postgres is unreachable or the row doesn't exist
	// yet (fresh dev environment), we fall back to YAML defaults so the
	// pod still boots.
	dspProfile, dspRow := loadDSPIdentity(cfg, profile, log)
	isCompetitor := dspRow.IsCompetitor()

	// noise_pct and no_bid_rate are TierLive — both should re-read per
	// request so SetConfigForPod can flip them at runtime (e.g. e2e tests
	// switching a DSP into "always no-bid" mode). The dspRow values from
	// loadDSPIdentity seed Postgres defaults via the registry but the
	// runtime path goes through the config map.
	noisePctFn := func() float64 {
		// Knobs.NoisePct reads "dsp.noise_pct" from cfg with default 0; we
		// fall back to the dspRow value if the key is unset (fresh DB).
		if v := cfg.GetFloat(keys.DSP.NoisePct.Key(), -1); v >= 0 {
			return v
		}
		return float64(dspRow.NoisePct)
	}
	noBidRateFn := func() float64 {
		if v := cfg.GetFloat(keys.DSP.NoBidRate.Key(), -1); v >= 0 {
			return v
		}
		return dspRow.NoBidRate
	}

	// Redis budget tracker.
	l2 := connectRedis(cfg, log)
	budget := NewBudgetTracker(l2, knobs.BudgetResetInterval.Value, log)
	shadingTracker := bidshading.NewTracker()

	// NATS bus for warm-cache invalidate subscription. nil-tolerant — the
	// warm cache degrades to poll-only mode if NATS is unreachable.
	bus := connectNATS(cfg, log)

	// Separate bus for outbound event publishing (BudgetDepletedEvent). Same
	// pattern as cmd/publisher-adserver — a dedicated *natsbus.Bus so we
	// can call EnsureStream (not on the interface) and wrap with Publisher.
	// Nil-tolerant: depletion events are best-effort observability, not the
	// hot path.
	natsURL := cfg.Get(keys.DSP.NATSURL.Key(), keys.Exchange.NATSURL.Get(cfg))
	var pub *events.Publisher
	if pubBus, err := natsbus.New(natsURL, constants.ServiceDSP+"-events", log); err == nil {
		pubBus.EnsureStream(context.Background(), events.StreamName, []string{events.StreamSubjects})
		pub = events.NewPublisher(pubBus, log)
		lc.OnShutdown("dsp-publisher", func(_ context.Context) error { return pubBus.Close() })
	} else {
		log.Warn("dsp event publisher unavailable, BudgetDepleted events will be skipped", "error", err)
	}

	// Per-pod dedup state for budget-exhaustion publishes. Without this,
	// every bid request hitting an exhausted campaign would publish a new
	// event — potentially hundreds per second per campaign. sync.Map
	// resets on pod restart; that's acceptable because true daily-budget
	// reset already triggers a budget counter reset in Redis, after which
	// the campaign would re-deplete and warrant a fresh event.
	var depletedAlreadyPublished sync.Map

	// Secrets warm cache. Loads the api_key + jwt_signing secrets this
	// service needs for management-endpoint auth. /readyz waits for first
	// successful load so the service won't accept auth-required traffic
	// before the cache has rows.
	secretsCache := secrets.Start(context.Background(), cfg, clk, log, constants.ServiceDSP)
	lc.OnShutdown("secrets-cache", func(_ context.Context) error { secretsCache.Stop(); return nil })
	hlth.AddReadinessCheck("secrets-cache", func(_ context.Context) error { return secretsCache.Ready() })

	// Postgres + warm cache for campaigns. dspID is the filter — only
	// campaigns under accounts where accounts.dsp_id matches this pod's
	// DSP make it into the cache. accountIDs allowlist is kept as a
	// fallback for the YAML-only boot path (Postgres unreachable).
	accountIDs := derivedAccountUUIDs(dspProfile)
	campaignCache := startCampaignCache(cfg, clk, log, dspProfile, bus, dspRow.ID, accountIDs)
	if campaignCache != nil {
		lc.OnShutdown("campaign-cache", func(_ context.Context) error { campaignCache.Stop(); return nil })
	}

	// Reconcile pacing budget counters to the billing engine's committed-spend
	// snapshots (broadcast by reporting). Keeps the local win-notice decrement
	// as the intra-snapshot guard while correcting phantom-win / CPC over-count.
	if err := startPacingReconcile(context.Background(), bus, campaignCache, budget, cfg, log); err != nil {
		// Self-heal, don't latch: a boot race with NATS/JetStream would fail
		// this Subscribe once and leave pacing reconcile DEAF (phantom-win /
		// CPC over-count never corrected) until a manual restart. Retry until
		// it sticks — same doctrine as webhooks/notifications.
		log.Error("pacing spend reconcile subscribe failed, will retry", "error", err)
		go func() {
			for {
				time.Sleep(15 * time.Second)
				if err := startPacingReconcile(context.Background(), bus, campaignCache, budget, cfg, log); err == nil {
					log.Info("pacing spend reconcile established after retry")
					return
				}
			}
		}()
	}

	// Path B: DSP-private audience segments. Looked up per bid request and
	// unioned with SSP-stamped segments before targeting evaluation. Nil
	// store = no enrichment, DSP keeps bidding on whatever the SSP sent.
	audienceStore, audiencePreloader, audienceStop := openAudienceStore(cfg, l2, log)
	// Membership invalidates → near-immediate preloader refresh (same
	// wiring as the SSP): the DSP's private-segment union sees uploads /
	// profile-builder output in seconds, not the 30s poll.
	if bus != nil && audiencePreloader != nil {
		audiencePreloader.SubscribeInvalidate(context.Background(), bus, constants.ServiceDSP)
	}
	lc.OnShutdown("audience-store", func(_ context.Context) error { audienceStop(); return nil })

	// Consent / opt-out registry warm cache — enforced on the bid path so
	// the DSP doesn't bid on (or personalise to) opted-out users.
	optOutCache := startOptOutCache(cfg, clk, log, bus)
	if optOutCache != nil {
		lc.OnShutdown("opt-out-cache", func(_ context.Context) error { optOutCache.Stop(); return nil })
	}

	// Prepay balance gate (money loop) — no funds, no bid, account-wide.
	// Warm cache of advertiser_balances + Redis win mirror; see balance.go.
	balanceGate, balanceCache := startBalanceGate(cfg, clk, log, bus, l2)
	if balanceCache != nil {
		lc.OnShutdown("balance-cache", func(_ context.Context) error { balanceCache.Stop(); return nil })
	}

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
	adCertKeyFn := adCertKeySource(cfg, log, func(name string, fn func()) {
		lc.OnShutdown(name, func(_ context.Context) error { fn(); return nil })
	})
	adCertVerify := adCertVerifierFn(cfg, log, clk.Now, adCertKeyFn)
	identityResolver, identityStop := openIdentityResolver(cfg, log)
	lc.OnShutdown("identity-resolver", func(_ context.Context) error { identityStop(); return nil })
	identityMaxLinked := keys.DSP.IdentityMaxLinked.Get(cfg)
	bid := bidHandler(log, clk, campaignCache, audienceStore, optOutCache, budget, balanceGate, isCompetitor, noisePctFn, noBidRateFn, pub, &depletedAlreadyPublished, adCertVerify, identityResolver, identityMaxLinked)
	mux.HandleFunc(routes.OpenRTBBid, bid)
	// Internal gRPC twin of the bid endpoint. Only our own exchange dials it
	// (grpc://dsp-internal:8182); the exchange's fan-out to any third-party
	// DSP stays OpenRTB HTTP. Competitor-profile pods also listen but nothing
	// dials them over gRPC — their endpoints stay http:// to keep the
	// industry-standard path exercised in every auction.
	startInternalGRPC(lc, cfg, log, metrics, bid)

	mux.HandleFunc(routes.OpenRTBWin, winHandler(log, budget, balanceGate, campaignCache, shadingTracker))
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
	// Management CRUD endpoints require X-API-Key — validated against the
	// secrets warm cache via middleware.AuthAPIKey. /v1/openrtb/bid stays
	// open (exchange→DSP call is internal). Phase 5 will add the same
	// gate to the bid endpoint via service_s2s shared secrets.
	auth := middleware.AuthAPIKey(secretsCache, log)
	mux.Handle(routes.DSPCampaigns+"/", auth(http.HandlerFunc(campaignByIDHandler(mgmtDB, bus, accountIDs, log))))
	mux.Handle(routes.DSPCampaigns, auth(http.HandlerFunc(campaignsCollectionHandler(campaignCache, mgmtDB, bus, newDSPIdentityResolver(cfg, profile, dspRow, log), budget, log))))

	// Cache refresh stays debug-gated — it's purely a dev/test helper for
	// forcing a synchronous reload, not a customer-facing operation.
	if keys.Debug.EndpointsEnabled.Get(cfg) {
		refreshables := []warm.Refreshable{campaignCache, secretsCache.Cache}
		if optOutCache != nil {
			refreshables = append(refreshables, optOutCache)
		}
		if balanceCache != nil {
			refreshables = append(refreshables, balanceCache)
		}
		mux.HandleFunc(routes.DebugCacheRefresh, warm.RefreshHandler(refreshables...))
		if audiencePreloader != nil {
			mux.HandleFunc(routes.DebugAudienceRefresh, audienceRefreshHandler(audiencePreloader, log))
		}
		mux.HandleFunc(routes.DebugDSPBudget, dspBudgetDebugHandler(budget))
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

// dspSeedOverrides returns the per-pod boot-seed overrides for this DSP's
// profile-specific knobs. Source of truth order: YAML profile (for host
// dev where ./profiles is on disk) → Postgres `dsps` row (for the k8s
// pod image where YAML files aren't packaged) → nil (registry uses
// schema defaults). The k8s path matters: without it the schema default
// of 0 wins on first boot, and noisePctFn returns 0 forever — every
// competitor bid is deterministic and one creative wins every auction.
// Only runs at boot; per-pod row only gets the value on first-ever boot
// per registry's exists-check semantics.
func dspSeedOverrides(profileName string, log *slog.Logger) map[string]string {
	if profile, err := FindProfile(profileName, log); err == nil {
		return map[string]string{
			keys.DSP.NoisePct.Key():  strconv.FormatFloat(float64(profile.NoisePct), 'f', -1, 64),
			keys.DSP.NoBidRate.Key(): strconv.FormatFloat(profile.NoBidRate, 'f', -1, 64),
		}
	}
	if row := dspRowFromEnvDB(profileName, log); row != nil {
		log.Info("profile YAML not found, seeded from postgres dsps row", "profile", profileName, "noise_pct", row.NoisePct, "no_bid_rate", row.NoBidRate)
		return map[string]string{
			keys.DSP.NoisePct.Key():  strconv.Itoa(row.NoisePct),
			keys.DSP.NoBidRate.Key(): strconv.FormatFloat(row.NoBidRate, 'f', -1, 64),
		}
	}
	log.Warn("profile YAML and postgres dsps row both unavailable, skipping seed-default override", "profile", profileName)
	return nil
}

// dspRowFromEnvDB does a one-shot Postgres lookup for the dsps row at
// seed time (before pkg/config has fully wired up). Reads DATABASE_URL
// directly from env so it doesn't depend on cfg, which is what we're
// trying to seed. Returns nil on any error so the caller can fall back.
func dspRowFromEnvDB(profileName string, log *slog.Logger) *postgres.DSPRow {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return nil
	}
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Debug("seed dsps lookup: open failed", "error", err)
		return nil
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	row, err := postgres.DSPByName(ctx, db, profileName)
	if err != nil {
		log.Debug("seed dsps lookup: row not found", "profile", profileName, "error", err)
		return nil
	}
	return row
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

	dbURL := cfg.Get(keys.Database.URL.Key(), "")
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

// newDSPIdentityResolver returns a func that reports this pod's dsps-row
// identity (id, name), re-querying Postgres when the boot-time resolution
// came back empty. Without this, a pod that booted before Postgres (or
// before the seed) latched ID="" in the campaign-create handler forever —
// "this dsp has no dsps row" 400s until a manual restart. Re-resolution is
// throttled to one attempt per 10s; once an ID is found it's cached for the
// pod's lifetime (a dsps row's ID never changes).
func newDSPIdentityResolver(cfg *config.Config, profileName string, boot *postgres.DSPRow, log *slog.Logger) func() (string, string) {
	var mu sync.Mutex
	id, name := boot.ID, boot.Name
	var lastAttempt time.Time
	return func() (string, string) {
		mu.Lock()
		defer mu.Unlock()
		if id != "" || time.Since(lastAttempt) < 10*time.Second {
			return id, name
		}
		lastAttempt = time.Now()
		dbURL := cfg.Get(keys.Database.URL.Key(), "")
		if dbURL == "" {
			return id, name
		}
		db, err := sql.Open("postgres", dbURL)
		if err != nil {
			return id, name
		}
		defer db.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		row, err := postgres.DSPByName(ctx, db, profileName)
		if err != nil {
			log.Warn("dsps row still unresolved; will retry on demand", "profile", profileName, "error", err)
			return id, name
		}
		id, name = row.ID, row.Name
		log.Info("dsps row resolved after boot (self-heal)", "name", name, "id", id)
		return id, name
	}
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
// Returns (Lookup, preloader-or-nil, stopFn). The preloader is non-nil
// only when the warm-preload variant is the active backend — the debug
// /debug/audience/refresh endpoint uses it to force a sync refresh
// without waiting for the 30s tick. Other backends (postgres-direct,
// lazy cache) need no manual refresh because they always read fresh.
func openAudienceStore(cfg *config.Config, l2 cache.L2Cache, log *slog.Logger) (audstore.Lookup, *audpreload.Preloader, func()) {
	dbURL := cfg.Get(keys.Database.URL.Key(), "")
	if dbURL == "" {
		log.Warn("database.url not set, dsp audience store disabled")
		return nil, nil, func() {}
	}
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		// sql.Open only validates the DSN; it does not connect. A real
		// error here means a malformed URL — genuinely unusable.
		log.Warn("dsp audience store open failed", "error", err)
		return nil, nil, func() {}
	}
	// NO boot-time ping gate: sql.Open is lazy, and a cold boot races
	// Postgres DNS/readiness (observed 2026-07-19 clean-slate — the ping
	// failed with "lookup postgres: no such host" and DISABLED audience
	// segments for the pod's entire life; every segment-targeted campaign
	// silently no-bid). The pool connects on first use and the preloader's
	// loop retries forever, so construct unconditionally and degrade
	// gracefully until Postgres answers.
	if l2 == nil {
		log.Info("dsp audience store connected (postgres-direct, no L2 cache)")
		return audiencepg.New(db), nil, func() { _ = db.Close() }
	}
	interval := keys.Audience.PreloadInterval.Get(cfg)
	ttl := keys.Audience.CacheTTL.Get(cfg)
	pre := audpreload.New(audpreload.Config{DB: db, L2: l2, Interval: interval, TTL: ttl, Log: log})
	if err := pre.Start(context.Background()); err != nil {
		log.Warn("audience preloader start failed, falling back to lazy cache", "error", err)
		return audcached.New(audiencepg.New(db), l2, ttl, log), nil, func() { _ = db.Close() }
	}
	log.Info("dsp audience store connected (redis warm preload)", "interval", interval, "ttl", ttl)
	return pre, pre, func() { pre.Stop(); _ = db.Close() }
}

// startCampaignCache wires the warm cache to Postgres, or to a YAML-derived
// memory loader if Postgres is unreachable. Either way the bid handler reads
// from a *warm.Cache[models.Campaign]. Bus is passed in (rather than dialled
// here) so other subscribers in the service can share one NATS connection.
func startCampaignCache(cfg *config.Config, clk clock.Clock, log *slog.Logger, profile *DSPProfile, bus events.EventBus, dspID string, accountIDs []string) *warm.Cache[models.Campaign] {
	pollInterval := firstNonZeroDuration(
		cfg.GetDuration(keys.DSP.WarmCampaignsPollInterval.Key(), 0),
		keys.CacheWarm.PollInterval.Get(cfg),
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

// startOptOutCache builds the user opt-out registry warm cache used to
// enforce consent on the bid hot path. Returns nil when database.url is
// unset — in that case the bid handler fails open (bids as if no opt-outs
// exist), consistent with the platform's other "boot regardless of infra"
// caches. An opted-out user is a small minority, so failing open on a
// missing DB favours availability; the next poll repopulates the cache.
func startOptOutCache(cfg *config.Config, clk clock.Clock, log *slog.Logger, bus events.EventBus) *warm.Cache[privacy.OptOut] {
	dbURL := cfg.Get(keys.Database.URL.Key(), "")
	if dbURL == "" {
		log.Warn("database.url not set, opt-out enforcement disabled (no consent cache)")
		return nil
	}
	pollInterval := firstNonZeroDuration(
		cfg.GetDuration(keys.DSP.WarmOptOutsPollInterval.Key(), 0),
		keys.CacheWarm.PollInterval.Get(cfg),
	)
	loader := &warm.RetryingLoader[privacy.OptOut]{
		Log:   log,
		KeyFn: func(o privacy.OptOut) string { return o.UserID },
		Construct: func() (warm.Loader[privacy.OptOut], error) {
			store, err := postgres.New(postgres.Config{PrimaryURL: dbURL, MaxOpenConns: 5, MaxIdleConns: 2, ConnMaxLifetime: 5 * time.Minute})
			if err != nil {
				return nil, fmt.Errorf("postgres connect: %w", err)
			}
			return &postgres.OptOutLoader{Store: store}, nil
		},
	}
	c := warm.New(warm.Config[privacy.OptOut]{
		Name:              "opt_outs",
		Loader:            loader,
		Clock:             clk,
		Bus:               bus,
		InvalidateSubject: events.SubjectCacheInvalidateOptOuts,
		PollInterval:      pollInterval,
		Log:               log,
	})
	if err := c.Start(context.Background()); err != nil {
		log.Error("opt-out cache initial load failed", "error", err)
	}
	return c
}

// pickCampaignLoader returns a self-healing warm.Loader. The DSP keeps
// its YAML-derived fallback when database.url is unset (so a fully
// offline boot still produces some campaigns to bid with), but when a
// URL is configured we wrap the Postgres path with RetryingLoader so a
// transient Postgres outage doesn't pin the cache for the process
// lifetime — the next 30s poll reconnects.
//
// DSPID is the primary filter (joins accounts.dsp_id). AccountIDs is
// kept as a YAML-derived fallback for environments where the dsps row
// doesn't exist yet or migration 022 hasn't run.
func pickCampaignLoader(cfg *config.Config, log *slog.Logger, profile *DSPProfile, dspID string, accountIDs []string) warm.Loader[models.Campaign] {
	dbURL := cfg.Get(keys.Database.URL.Key(), "")
	if dbURL == "" {
		log.Warn("database.url not set, using YAML in-memory campaign loader")
		return &yamlCampaignLoader{profile: profile}
	}
	return &warm.RetryingLoader[models.Campaign]{
		Log:   log,
		KeyFn: func(c models.Campaign) string { return c.ID },
		Construct: func() (warm.Loader[models.Campaign], error) {
			store, err := postgres.New(postgres.Config{PrimaryURL: dbURL, MaxOpenConns: 5, MaxIdleConns: 2, ConnMaxLifetime: 5 * time.Minute})
			if err != nil {
				return nil, fmt.Errorf("postgres connect: %w", err)
			}
			id := dspID
			if id == "" {
				// Boot raced Postgres/seed: loadDSPIdentity fell back to the
				// YAML row, whose ID is empty. Re-resolve EVERY construct —
				// and refuse to build a loader without a dsps-row scope:
				//   - unscoped (no id, no allowlist) loads ALL campaigns; at
				//     the Helm cutover an unscoped competitor pod bid (and
				//     won) other DSPs' campaigns.
				//   - the YAML accountIDs allowlist is just as wrong long-term:
				//     it silently pins the cache to seed-era accounts, so every
				//     campaign created later under a fresh account is INVISIBLE
				//     — zero rows, zero errors, no self-heal. Found live on
				//     2026-07-26: three wedged pods no-bid every e2e world (36
				//     test failures) after booting before Postgres DNS existed.
				// Returning an error keeps the cache EMPTY (no bids) and
				// RetryingLoader retries next poll, so the pod heals the
				// moment the dsps row is reachable.
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				row, rerr := postgres.DSPByName(ctx, store.Read(), profile.Name)
				if rerr != nil {
					store.Close()
					log.Error("dsp identity unresolved; refusing mis-scoped campaign load (no bids until the dsps row is readable)",
						"profile", profile.Name, "error", rerr)
					return nil, fmt.Errorf("dsp identity unresolved for %q: %w", profile.Name, rerr)
				}
				id = row.ID
				log.Info("dsp identity re-resolved for campaign loader", "profile", profile.Name, "dsp_id", id)
			}
			return &postgres.CampaignLoader{Store: store, DSPID: id, AccountIDs: accountIDs}, nil
		},
	}
}

// connectNATS returns a JetStream-backed bus if reachable, else nil
// (warm cache falls back to poll-only mode).
func connectNATS(cfg *config.Config, log *slog.Logger) events.EventBus {
	url := cfg.Get(keys.DSP.NATSURL.Key(), keys.Exchange.NATSURL.Get(cfg))
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
		c.Location = models.ResolveLocation(c.Timezone) // non-nil (UTC) even for YAML fallback
		out = append(out, c)
	}
	return out, nil
}

func (l *yamlCampaignLoader) KeyOf(c models.Campaign) string { return c.ID }

// connectRedis returns a real Redis L2 cache if reachable, falling back
// to MemoryL2 with a warning so dev environments without Redis still boot.
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

func bidHandler(log *slog.Logger, clk clock.Clock, campaigns *warm.Cache[models.Campaign], audienceStore audstore.Lookup, optOut *warm.Cache[privacy.OptOut], budget *BudgetTracker, balanceGate *BalanceGate, isCompetitor bool, noisePctFn, noBidRateFn func() float64, pub *events.Publisher, depletedAlreadyPublished *sync.Map, adCertVerify func(*openrtb.BidRequest) (bool, string), identityResolver identityResolver, identityMaxLinked int) http.HandlerFunc {
	// balanceDepletedPublished dedups the account-level depleted event the
	// same way depletedAlreadyPublished dedups the campaign-level one.
	// Entries are cleared when the gate sees funds again, so a re-depletion
	// after a topup fires a fresh event.
	var balanceDepletedPublished sync.Map
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

		// ads.cert: verify the request was authentically signed by the exchange.
		// Strict mode no-bids an unsigned/tampered request; warn logs and bids;
		// off (default) skips. Reads enforcement live.
		if allow, reason := adCertVerify(&bidReq); !allow {
			nbr := openrtb.NBRAdCertInvalid
			if reason == "adcert_stale" {
				nbr = openrtb.NBRAdCertStale
			}
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			json.NewEncoder(w).Encode(openrtb.BidResponse{ID: bidReq.ID, NoBid: true, NBR: nbr, NBRReason: reason})
			reqLog.Info("no bid", "reason", reason)
			return
		}

		// Consent / opt-out gate. Combine the platform opt-out registry
		// (warm cache, keyed by user id) with the inbound OpenRTB
		// regulatory signals. A no-bid verdict short-circuits before any
		// targeting work; a no-personalise verdict strips behavioural
		// targeting below so only contextual signals are used.
		// Stable user key: the first-party User.ID, or the UID2 token from
		// User.EIDs when the request is cookieless. Used for opt-out, private
		// segment lookup, and (downstream) frequency capping so a UID2-only
		// user is still addressable.
		userKey := openrtb.UserKey(bidReq.User)
		optLevel := privacy.LevelNone
		if optOut != nil && userKey != "" {
			if rec, ok := optOut.ByID(userKey); ok {
				optLevel = rec.Level
			}
		}
		sig := privacy.Signals{Level: optLevel}
		if bidReq.Regs != nil {
			sig.COPPA = bidReq.Regs.COPPA
			if bidReq.Regs.Ext != nil {
				sig.GDPR = bidReq.Regs.Ext.GDPR
				sig.USPrivacy = bidReq.Regs.Ext.USPrivacy
				sig.GPP = bidReq.Regs.Ext.GPP
				sig.GPPSID = bidReq.Regs.Ext.GPPSID
				sig.GPC = bidReq.Regs.Ext.GPC == 1
			}
		}
		if bidReq.User != nil && bidReq.User.Ext != nil {
			sig.TCFConsent = bidReq.User.Ext.Consent
		}
		consent := privacy.Evaluate(sig)
		if !consent.Bid {
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			json.NewEncoder(w).Encode(openrtb.BidResponse{ID: bidReq.ID, NoBid: true})
			reqLog.Info("no bid", "reason", "privacy", "privacy_reason", consent.Reason)
			return
		}

		tReq := targeting.Request{
			Device:        deviceTypeStr(bidReq.Device),
			InventoryType: inventoryType(bidReq),
			Channel:       dspRequestChannel(&bidReq),
		}
		if bidReq.Device != nil {
			if bidReq.Device.Geo != nil {
				tReq.Geo = bidReq.Device.Geo.Country
			}
			tReq.OS = bidReq.Device.OS
		}
		if bidReq.Site != nil {
			tReq.Domain = bidReq.Site.Domain
			tReq.Categories = bidReq.Site.Cat
			// OpenRTB Site.keywords is a comma-separated string; the engine
			// matches against a slice of page keywords.
			if kw := strings.TrimSpace(bidReq.Site.Keywords); kw != "" {
				parts := strings.Split(kw, ",")
				for i := range parts {
					parts[i] = strings.TrimSpace(parts[i])
				}
				tReq.Keywords = parts
			}
			if len(tReq.Categories) == 0 {
				classifier := targeting.NewClassifier()
				tReq.Categories = classifier.Classify(bidReq.Site.Domain, bidReq.Site.Page, nil, nil)
			}
		}
		// Behavioural segments only when the consent verdict allows
		// personalisation; otherwise the bid proceeds on contextual
		// signals (geo / domain / category) alone.
		if consent.Personalise && bidReq.User != nil && bidReq.User.Ext != nil {
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
		if consent.Personalise && audienceStore != nil && userKey != "" {
			lookupCtx, cancel := context.WithTimeout(r.Context(), 25*time.Millisecond)
			// Expands userKey via the identity graph when resolution is enabled,
			// so segments on a linked id (UID2/device/cross-publisher) also match.
			private := dspPrivateSegments(lookupCtx, audienceStore, identityResolver, userKey, identityMaxLinked, log)
			cancel()
			if len(private) > 0 {
				tReq.Segments = append(tReq.Segments, private...)
			}
		}

		// Household segments (CTV): the SSP carries a household id as an EID
		// (salted IP hash — the household proxy). Household-scoped audience
		// segments are ordinary audience_segment_members rows keyed by the
		// hh: id, so this is the same lookup as user segments, just under the
		// household key. Consent-gated identically — no consent, no household
		// personalisation. Same 25ms degrade-gracefully budget as above.
		if consent.Personalise && audienceStore != nil {
			if hhID := openrtb.HouseholdFrom(bidReq.User); hhID != "" {
				lookupCtx, cancel := context.WithTimeout(r.Context(), 25*time.Millisecond)
				hhSegs := dspPrivateSegments(lookupCtx, audienceStore, nil, hhID, 0, log)
				cancel()
				if len(hhSegs) > 0 {
					tReq.Segments = append(tReq.Segments, hhSegs...)
				}
			}
		}

		floor := 0.0
		var reqW, reqH, reqMinDur, reqMaxDur int
		reqFormat := "display"
		if len(bidReq.Imp) > 0 {
			floor = bidReq.Imp[0].BidFloor
			switch {
			case bidReq.Imp[0].Video != nil:
				reqFormat = "video"
				reqW = bidReq.Imp[0].Video.W
				reqH = bidReq.Imp[0].Video.H
				reqMinDur = bidReq.Imp[0].Video.MinDuration
				reqMaxDur = bidReq.Imp[0].Video.MaxDuration
			case bidReq.Imp[0].Audio != nil:
				reqFormat = "audio"
				reqMinDur = bidReq.Imp[0].Audio.MinDuration
				reqMaxDur = bidReq.Imp[0].Audio.MaxDuration
			case bidReq.Imp[0].Native != nil:
				reqFormat = "native"
			case bidReq.Imp[0].Banner != nil:
				reqW = bidReq.Imp[0].Banner.W
				reqH = bidReq.Imp[0].Banner.H
			}
		}

		// Multi-winner channels (retail sponsored slots, in-game scene surfaces)
		// take a SLATE, not a single best bid: the exchange ranks/assigns every
		// eligible product across N slots (relevance × bid for retail; price +
		// competitive separation for in-game), so the DSP surfaces all of its
		// eligible products (grouped by advertiser seat) instead of pre-selecting
		// the highest bidder. Every other channel keeps the single-best-bid path.
		isSlate := len(bidReq.Imp) > 0 && bidReq.Imp[0].Ext != nil &&
			(bidReq.Imp[0].Ext.Channel == constants.ChannelRetail ||
				bidReq.Imp[0].Ext.Channel == constants.ChannelInGame)
		retailBySeat := map[string][]openrtb.BidObj{}
		var retailSeatOrder []string
		var retailCur string

		all := campaigns.All()
		var bestBid *openrtb.BidObj
		var bestCampaign *models.Campaign
		var bestPrice float64
		// pickedCreativeID tracks the size-matched creative for the bestBid;
		// holds across iterations because the chosen campaign comes with its
		// matched creative UUID, not the line item's generic CreativeID.
		var pickedCreativeID string

		// One timestamp for the whole request (consistent across all campaigns
		// and cheaper than recomputing per candidate). Time-of-day modifiers
		// shift it into each campaign's pre-resolved location below.
		reqNow := clk.Now()

		// Blocked advertiser domains (OpenRTB badv) — a campaign whose advertiser
		// domain the caller excluded must not bid. Powers CTV ad-pod competitive
		// separation: each pod sub-auction lists the advertisers already picked.
		var blockedAdv map[string]bool
		if len(bidReq.BAdv) > 0 {
			blockedAdv = make(map[string]bool, len(bidReq.BAdv))
			for _, d := range bidReq.BAdv {
				blockedAdv[strings.ToLower(d)] = true
			}
		}

		for i := range all {
			c := &all[i]
			if c.Status != constants.StatusLive {
				continue
			}
			if blockedAdv[strings.ToLower(c.CreativeDomain)] {
				continue // advertiser already in the pod (OpenRTB badv)
			}

			// Creative match: display creatives need a size match, video/
			// audio creatives need a format + duration-window match. Bid
			// response carries the matched creative's UUID + dimensions
			// + duration so the SSP / VAST builder don't have to look
			// them up again.
			match := selectCreativeForRequest(c, reqFormat, reqW, reqH, reqMinDur, reqMaxDur)
			if match == nil {
				reqLog.Debug("no matching creative",
					"campaign", c.ID,
					"want_format", reqFormat,
					"want", fmt.Sprintf("%dx%d / %d–%ds", reqW, reqH, reqMinDur, reqMaxDur))
				continue
			}
			crid := match.ID

			// Multi-winner channels treat the product category as a SOFT signal
			// (ranked/separated at the exchange), not a hard content filter — a shoe
			// ad stays eligible on a page browsing another category (it just ranks
			// lower), and an in-game product isn't excluded by the scene's category.
			// So drop category targeting for the slate channels; every other
			// dimension (geo/device/audience) still gates normally.
			tRules := c.Targeting
			if isSlate {
				tRules.Include.Categories = nil
				tRules.Exclude.Categories = nil
			}
			result := targeting.Evaluate(tRules, tReq)
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
			// Exhaustion check FIRST. The pacer's ShouldBid also returns
			// false when over-budget but conflates that with intra-day
			// throttling — if we fall into the pacer branch we never get
			// the chance to publish BudgetDepletedEvent. Exhaustion is a
			// stronger signal than throttling and deserves its own log +
			// event.
			if currentSpend >= c.DailyBudget {
				reqLog.Debug("campaign daily budget exhausted", "campaign", c.ID)
				// Publish BudgetDepletedEvent once per (campaign, pod
				// lifetime) so reporting + dashboards / alerts have a
				// signal beyond log scraping. LoadOrStore returns the
				// previous value's "exists" flag, so we publish only on
				// the first detection — without this guard, every bid
				// request hitting an exhausted campaign would emit a
				// fresh event (potentially hundreds/sec/campaign).
				if pub != nil {
					if _, already := depletedAlreadyPublished.LoadOrStore(c.ID, struct{}{}); !already {
						reqLog.Info("publishing budget depleted event", "campaign", c.ID, "spent", currentSpend, "budget", c.DailyBudget)
						go pub.BudgetDepleted(context.WithoutCancel(ctx), events.BudgetDepletedEvent{
							CampaignID: c.ID,
							AccountID:  c.AccountID,
							Budget:     c.DailyBudget,
							Spent:      currentSpend,
							Timestamp:  clk.Now(),
						})
					}
				}
				continue
			}
			// Not exhausted → re-arm the one-shot depleted event for this
			// campaign. Without this the event fires once per POD lifetime:
			// a new day (spend resets), a raised budget, or a reconcile-down
			// all silently skip the next depletion. Mirrors the balance
			// gate's re-arm-when-funds-return posture.
			depletedAlreadyPublished.Delete(c.ID)
			if !pacer.ShouldBid(currentSpend) {
				reqLog.Debug("campaign throttled by pacing", "campaign", c.ID, "spend", currentSpend)
				continue
			}
			// Prepay balance gate (money loop): after the campaign-level
			// budget checks, the ACCOUNT must have funds. Fail-open when the
			// gate isn't wired (no DB) — same posture as opt-outs.
			if balanceGate != nil {
				if ok, remaining := balanceGate.HasFunds(c.AccountID); !ok {
					reqLog.Info("no bid", "reason", "balance_depleted", "campaign", c.ID, "account", c.AccountID)
					if pub != nil {
						if _, already := balanceDepletedPublished.LoadOrStore(c.AccountID, struct{}{}); !already {
							go pub.BalanceDepleted(context.WithoutCancel(ctx), events.BalanceDepletedEvent{
								AccountID: c.AccountID,
								Balance:   remaining,
								Timestamp: clk.Now(),
							})
						}
					}
					continue
				}
				// Funds present: re-arm the depleted event for this account
				// so a future re-depletion (post-topup) fires again.
				balanceDepletedPublished.Delete(c.AccountID)
			}

			// Time-of-day modifiers evaluate the hour in the campaign's
			// timezone. c.Location is pre-resolved at cache-load (UTC when the
			// timezone is unset/unparseable), so the bid path does no per-bid
			// time.LoadLocation — just an in-memory .In() shift.
			campaignNow := reqNow
			if c.Location != nil {
				campaignNow = reqNow.In(c.Location)
			}
			// Segments ride along so Modifiers.Audience actually prices bids —
			// tReq.Segments is the consent-gated union (public stamp + DSP
			// private + household), so an opted-out user simply has none and
			// no audience modifier applies. (This was the "dead audience
			// modifiers" gap: ModifierContext was built without Segments, so
			// segment bid modifiers never fired on any bid.)
			modCtx := targeting.ModifierContext{
				Device: tReq.Device, GeoCountry: tReq.Geo,
				HourOfDay: campaignNow.Hour(),
				Segments:  tReq.Segments,
			}
			adjustedBid, _ := targeting.ApplyModifiers(c.BaseBid, c.Modifiers, modCtx)

			if isCompetitor {
				noBidRate := noBidRateFn()
				if rand.Float64() < noBidRate {
					reqLog.Debug("competitor random no-bid", "campaign", c.ID)
					continue
				}
				noisePct := noisePctFn()
				noiseFraction := noisePct / 100.0
				noiseMin := 1.0 - noiseFraction
				noiseRange := noiseFraction * 2.0
				adjustedBid *= noiseMin + rand.Float64()*noiseRange
			}

			if adjustedBid < floor {
				reqLog.Debug("bid below floor", "campaign", c.ID, "bid", adjustedBid, "floor", floor)
				continue
			}

			// Build the candidate bid once. Cat carries the sponsored product's IAB
			// category so the exchange can score retail relevance (product category
			// vs the shopper's browsed categories); harmless on other channels.
			cand := &openrtb.BidObj{
				ID:       "bid-" + bidReq.ID + "-" + c.ID,
				ImpID:    bidReq.Imp[0].ID,
				Price:    adjustedBid,
				CID:      c.ID,
				CrID:     crid,
				ADomain:  []string{c.CreativeDomain},
				BidModel: c.BidModel,
				W:        match.Width,
				H:        match.Height,
				Dur:      match.Duration,
				MediaURL: match.MediaURL,
				Cat:      append([]string(nil), c.Targeting.Include.Categories...),
			}
			// Native creatives carry their markup in AdM: an OpenRTB Native
			// response built from the creative's asset set. Impression/click
			// trackers are injected downstream (like banner HTML / VAST),
			// so none are added here.
			if reqFormat == "native" && match.Native != nil {
				na := match.Native
				resp := native.BuildResponse(native.AssetSet{
					Title:      na.Title,
					MainImage:  na.MainImage,
					MainImageW: na.MainImageW,
					MainImageH: na.MainImageH,
					Icon:       na.Icon,
					Sponsored:  na.Sponsored,
					Body:       na.Body,
					CTA:        na.CTA,
					LandingURL: na.LandingURL,
				}, nil, nil)
				if adm, err := native.MarshalResponse(resp); err == nil {
					cand.AdM = adm
				} else {
					reqLog.Error("native response marshal failed", "campaign", c.ID, "error", err)
				}
			}

			// Slate channels: every eligible product goes into the slate (grouped
			// by seat) for the exchange to rank/assign across slots.
			if isSlate {
				if _, seen := retailBySeat[c.AccountID]; !seen {
					retailSeatOrder = append(retailSeatOrder, c.AccountID)
				}
				retailBySeat[c.AccountID] = append(retailBySeat[c.AccountID], *cand)
				if retailCur == "" {
					retailCur = c.Currency
				}
			}

			if adjustedBid > bestPrice {
				bestPrice = adjustedBid
				bestCampaign = c
				pickedCreativeID = crid
				bestBid = cand
			}
		}

		// Slate response: one SeatBid per advertiser, all eligible products. The
		// exchange ranks/assigns across slots (relevance for retail, competitive
		// separation for in-game).
		if isSlate && len(retailSeatOrder) > 0 {
			seatBids := make([]openrtb.SeatBid, 0, len(retailSeatOrder))
			for _, seat := range retailSeatOrder {
				seatBids = append(seatBids, openrtb.SeatBid{Seat: seat, Bid: retailBySeat[seat]})
			}
			if retailCur == "" {
				retailCur = "USD"
			}
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			json.NewEncoder(w).Encode(openrtb.BidResponse{ID: bidReq.ID, Cur: retailCur, SeatBid: seatBids})
			reqLog.Info("slate bid", "channel", bidReq.Imp[0].Ext.Channel, "seats", len(seatBids), "products", len(all))
			return
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
			"creative", pickedCreativeID,
			"size", fmt.Sprintf("%dx%d", reqW, reqH),
		)
	}
}

// selectCreativeForRequest picks a creative variant that matches the
// current bid request. Returns nil when no variant fits — the caller
// no_bids on this request rather than serving a mismatched creative.
//
// Match rules:
//   - display: format=="display" and Width×Height equals reqW×reqH
//   - video:   format=="video" and Duration is within [minDur, maxDur]
//     (W/H are NOT required to match; players letterbox/scale
//     video creatives to fit slots, and our seed video sizes
//     are coarse — 640x360 standard renders fine in any video
//     slot at HD or below)
//   - audio:   format=="audio" and Duration within [minDur, maxDur]
//
// For zero-or-missing constraints (e.g. the request didn't set a
// max duration) we treat the constraint as "any" rather than zero.
func selectCreativeForRequest(c *models.Campaign, format string, reqW, reqH, minDur, maxDur int) *models.CampaignCreative {
	for i := range c.Creatives {
		cv := &c.Creatives[i]
		cvFmt := cv.Format
		if cvFmt == "" {
			cvFmt = "display"
		}
		if cvFmt != format {
			continue
		}
		switch format {
		case "display":
			if reqW == 0 || reqH == 0 {
				return cv
			}
			if cv.Width == reqW && cv.Height == reqH {
				return cv
			}
		case "video", "audio":
			if minDur > 0 && cv.Duration < minDur {
				continue
			}
			if maxDur > 0 && cv.Duration > maxDur {
				continue
			}
			if cv.MediaURL == "" {
				continue
			}
			return cv
		case "native":
			// A native creative needs its asset set with at least a title
			// (the one always-required element besides the image).
			if cv.Native == nil || cv.Native.Title == "" {
				continue
			}
			return cv
		}
	}
	// Display: legacy CreativeID fallback when no Creatives are loaded
	// AND no specific size was requested. Matches the historical
	// selectCreativeForSize behaviour so nothing else regresses.
	if format == "display" && reqW == 0 && reqH == 0 && len(c.Creatives) == 0 && c.CreativeID != "" {
		return &models.CampaignCreative{ID: c.CreativeID, Format: "display"}
	}
	return nil
}

// selectCreativeForSize returns the UUID of a creative whose declared
// width × height match the bid request's banner size, or "" when no
// match exists. Walks the line item's Creatives slice (already ordered
// by line_item_creatives.weight DESC, so the first match is the highest-
// weight variant for that size). When the bid request carries no size
// info (reqW=0 || reqH=0) we fall back to the line item's primary
// creative — necessary so non-display channels (which don't have w/h)
// keep bidding.
func selectCreativeForSize(c *models.Campaign, reqW, reqH int) string {
	if reqW == 0 || reqH == 0 {
		return c.CreativeID
	}
	for _, cv := range c.Creatives {
		if cv.Width == reqW && cv.Height == reqH {
			return cv.ID
		}
	}
	return ""
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

// dspRequestChannel derives the request's channel for channel targeting, mirroring
// the exchange's channelForRequest: the emerging channels (dooh/retail/ingame)
// ride imp.ext.channel (banner-shaped), everything else is read from the media
// object. Empty request → display.
func dspRequestChannel(req *openrtb.BidRequest) string {
	if len(req.Imp) == 0 {
		return constants.ChannelDisplay
	}
	imp := req.Imp[0]
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

// winHandler processes win notifications from the exchange (the OpenRTB
// nurl path). Updates the campaign budget counter and feeds the bid shading
// model. Single source of budget truth for this DSP: internal and external
// DSPs alike are notified through this endpoint. The parallel NATS
// adtech.auction.win event is for non-DSP consumers (reporting analytics,
// future billing ledger), not for re-driving the DSP's own budget.
func winHandler(log *slog.Logger, budget *BudgetTracker, balanceGate *BalanceGate, campaigns *warm.Cache[models.Campaign], tracker *bidshading.Tracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		bidID := q.Get("bid_id")
		price, _ := strconv.ParseFloat(q.Get("price"), 64)
		placementID := q.Get("placement_id")
		campaignID := q.Get("campaign_id")

		if campaignID != "" {
			// price is the auction CPM (OpenRTB bid.price = cost per 1000
			// impressions). The budget + balance meters track realized
			// per-impression DOLLARS, so book CPM/1000 — a $5.00 CPM win
			// spends $0.005, not $5.00. This keeps the local over-count meter
			// in the same units as the billing engine's committed-spend
			// snapshot it reconciles against.
			impCost := price / 1000
			budget.Record(campaignID, impCost)
			// Mirror the spend into the account-level balance counter so
			// the prepay gate sees it before the billing drawdown lands in
			// Postgres (money loop).
			if balanceGate != nil && campaigns != nil {
				if c, ok := campaigns.ByID(campaignID); ok {
					balanceGate.RecordWin(c.AccountID, impCost)
				}
			}
		}
		if placementID != "" {
			// Bid-shading reasons in CPM rates, not booked dollars — pass the
			// raw clearing CPM.
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

// audienceRefreshHandler exposes the audience preloader's sync-refresh
// method. Used by the e2e harness after inserting audience_segment_members
// rows so the bid path sees the new mapping immediately. Gated by
// debug.endpoints_enabled in the caller; this function trusts the
// gate has already been checked.
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

// dspBudgetDebugHandler returns a campaign's current daily spend counter in
// micro-dollars — the value the pacing gate reads and the spend-snapshot
// reconcile overwrites. GET /debug/budget?campaign_id=…. Lets the e2e harness
// observe pacing/reconcile at the DSP (the counter is shared in Redis across
// all replicas, so any replica answers authoritatively). Gated in the caller.
func dspBudgetDebugHandler(budget *BudgetTracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		campaignID := r.URL.Query().Get("campaign_id")
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		if campaignID == "" {
			http.Error(w, `{"error":"campaign_id required"}`, http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"campaign_id":  campaignID,
			"spent_micros": budget.SpendMicros(campaignID),
		})
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
