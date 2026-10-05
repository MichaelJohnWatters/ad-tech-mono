// cmd/gateway is the API Gateway and dashboard server.
// Single entry point for all REST/dashboard traffic.
// Handles auth (JWT/RBAC), proxies API calls to internal services,
// and serves the HTMX dashboard.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/accountexport"
	accountexportpg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/accountexport/postgres"
	accountlifecyclepg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/accountlifecycle/postgres"
	audiencepg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/audiencemappings"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/audit"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache"
	cacheredis "github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache/redis"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache/warm"
	catalogpg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/catalog/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/changelog"
	changelogpg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/changelog/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/dataproviders"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events/natsbus"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/health"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ingest"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ingestjobs"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/kubeops"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	marketplacepg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/marketplace/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/notifications"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/partner"
	partnerpg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/partner/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/pgp"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/pipeline"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/reporting"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/reportjobs"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/sdkasset"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/secrets"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ssoauth"
	ssoauthpg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/ssoauth/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/statuspage"
	statuspagepg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/statuspage/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/support"
	supportpg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/support/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
	_ "github.com/lib/pq"
)

func main() {
	log := logger.New(constants.ServiceGateway)
	sc := config.Setup(constants.ServiceGateway, keys.GatewaySchema(), log)
	cfg := sc.Cfg
	cfgMgr := sc.Manager
	hlth := health.New()
	lc := lifecycle.New(log)

	// The config-manager UI reads the union of every running pod's schema
	// from service_registry.schema_entries — no separate "load published
	// schema" step is needed here.
	dbURL := cfg.Get(keys.Database.URL.Key(), "")

	// Open a Postgres handle for handlers that need direct DB access
	// (bootstrap mints a secrets row, reset-and-reseed truncates).
	// sql.Open is lazy — it parses the URL but doesn't dial — so we
	// store the handle even if the first Ping fails. Postgres DNS may
	// not be ready when gateway boots (initial cluster spin-up); the
	// handle will succeed on subsequent requests once postgres is up.
	// Handlers still defend against a nil handle and 503 if DB never
	// becomes reachable.
	var gwDB *sql.DB
	if dbURL != "" {
		if d, err := sql.Open("postgres", dbURL); err == nil {
			gwDB = d
			lc.OnShutdown("gw-db", func(_ context.Context) error { return d.Close() })
			if err := d.Ping(); err != nil {
				log.Warn("gateway db ping failed at boot; handlers will retry on demand", "error", err)
			} else {
				// The gateway's main pool carries most tenant-scoped writes, so it
				// MUST connect as the NOBYPASSRLS app role or RLS is silently off.
				postgres.LogRLSEnforcement(context.Background(), d, log)
			}
		} else {
			log.Warn("gateway db open failed; bootstrap endpoint will return 503", "error", err)
		}
	}

	// OpenTelemetry — empty endpoint disables tracing so dev/test envs
	// without Jaeger still boot. Same pattern as the auction-path services.
	otelShutdown := tracing.Init(context.Background(), tracing.Config{
		ServiceName:    constants.ServiceGateway,
		ServiceVersion: keys.Otel.ServiceVersion.Get(cfg),
		Endpoint:       keys.Otel.Endpoint.Get(cfg),
		SampleRatio:    keys.Otel.SampleRatio.Get(cfg),
		Log:            log,
	})
	lc.OnShutdown("otel", func(ctx context.Context) error { return otelShutdown(ctx) })

	port := keys.Gateway.Port.Get(cfg)

	// Secrets warm cache — the source of truth for the JWT signing key (and
	// the operator API keys the /v1/api handlers validate). Started here (not
	// at its later use site) so the signing key is resolved before the auth
	// middleware is built. Start does a synchronous initial load, so an
	// active jwt_signing secret in Postgres is available immediately.
	secretsCache := secrets.Start(context.Background(), cfg, clock.Real{}, log, constants.ServiceGateway)
	lc.OnShutdown("gateway-secrets-cache", func(_ context.Context) error { secretsCache.Stop(); return nil })

	// JWT signing key precedence: active jwt_signing secret > config key >
	// empty. Empty means the dev auth-bypass (every request gets admin
	// claims) — allowed only when gateway.require_auth is false.
	signingKey := keys.Gateway.JwtSigningKey.Get(cfg)
	if sec, ok := secretsCache.LookupActiveByPurpose(secrets.PurposeJWTSigning); ok {
		signingKey = sec.Value
		log.Info("jwt signing key loaded from secrets store", "name", sec.Name)
	}
	if signingKey == "" {
		if keys.Gateway.RequireAuth.Get(cfg) {
			log.Error("gateway.require_auth=true but no JWT signing key is available (no active jwt_signing secret, empty gateway.jwt_signing_key); refusing to boot with auth bypassed")
			os.Exit(1)
		}
		log.Warn("SECURITY: no JWT signing key configured — auth is BYPASSED, every request receives admin claims. Dev only; set a jwt_signing secret (or gateway.require_auth=true) in staging/prod.")
		// Self-heal: the key is captured by value in every handler closure, so
		// a gateway that booted before Postgres (whole-VM boot) latched the
		// dev-bypass forever even though the secrets cache reconnects and the
		// secret EXISTS. When the cache later surfaces a jwt_signing secret,
		// exit — k8s/Tilt restarts the pod, which boots with real auth. A dev
		// stack with genuinely no secret never trips this (nothing appears).
		go func() {
			for range time.Tick(15 * time.Second) {
				if sec, ok := secretsCache.LookupActiveByPurpose(secrets.PurposeJWTSigning); ok && sec.Value != "" {
					log.Error("jwt_signing secret appeared after a bypass-mode boot — exiting so the restart boots with real auth", "name", sec.Name)
					os.Exit(1)
				}
			}
		}()
	}

	// Internal service URLs (configurable for staging/prod)
	dspURL := keys.Gateway.DSPURL.Get(cfg)
	sspURL := keys.Gateway.SSPURL.Get(cfg)
	adserverURL := keys.Gateway.AdserverURL.Get(cfg)
	pubadURL := keys.Gateway.PublisherAdServerURL.Get(cfg)
	reportingURL := keys.Gateway.ReportingURL.Get(cfg)
	exchangeURL := keys.Gateway.ExchangeURL.Get(cfg)
	trackerURL := keys.Gateway.TrackerURL.Get(cfg)
	publicTrackerURL := keys.Gateway.PublicTrackerURL.Get(cfg) // browser-reachable, for embedded pixels
	jaegerURL := keys.Gateway.JaegerURL.Get(cfg)
	// Object-store forwarding target for /v1/creatives/* — the
	// browser-reachable proxy for SVG / PNG / JPG assets stored in
	// Minio or S3. Default points at the in-cluster Minio service;
	// staging/prod overlays override with the real S3 endpoint.
	creativesStoreURL := cfg.Get(keys.Gateway.CreativesStoreURL.Key(), "http://"+routes.DefaultMinioEndpoint+"/"+keys.S3.Bucket.Get(cfg)+"/")

	// Shared Redis L2 (self-healing: fail-open from memory until Redis dials).
	// Backs session revocation and the optional distributed rate limiter — both
	// need cluster-shared state that survives a pod restart.
	gwL2 := cache.NewSelfHealingL2(func(ctx context.Context) (cache.L2Cache, error) {
		return cacheredis.New(ctx, cacheredis.Config{
			Addr:     keys.Redis.URL.Get(cfg),
			Password: keys.Redis.Password.Get(cfg),
			DB:       keys.Redis.DB.Get(cfg),
			PoolSize: keys.Redis.PoolSize.Get(cfg),
		})
	}, 10*time.Second, keys.Redis.URL.Get(cfg), log)
	lc.OnShutdown("gw-redis", func(context.Context) error { return gwL2.Close() })

	// Session revocation: a checkpoint per user; tokens issued before it are
	// rejected. Wired into the Auth middleware and threaded into portal page
	// gating. maxTokenLifetime = the login token's 12h expiry.
	revStore := middleware.NewRevocationStore(gwL2, 12*time.Hour, log)
	portalRevocation = revStore // browser page gates consult the same store
	// Data-residency home region: out-of-region accounts' mutations are rejected
	// (empty/default region = no gate, single-region deployments unaffected).
	authMiddleware := middleware.Auth(signingKey, log,
		middleware.WithRevocation(revStore),
		middleware.WithRegionGate(keys.Platform.Region.Get(cfg)))

	metrics := middleware.NewMetrics(constants.ServiceGateway)

	mux := http.NewServeMux()
	// On-demand profiler (internal mux only; zero cost until a profile is
	// pulled). Block/mutex profiling stays off until armed — see
	// middleware.SetProfileRates.
	middleware.AttachPprof(mux)

	// Health (no auth)
	mux.Handle(routes.Healthz, hlth.LivenessHandler())
	mux.Handle(routes.Readyz, hlth.ReadinessHandler())
	mux.Handle(routes.Metrics, metrics.Handler())

	// Static files (no auth). Cache-Control: no-cache forces the browser to
	// REVALIDATE before using a cached copy — the FileServer's ETag/Last-Modified
	// still yield cheap 304s when unchanged, but a deploy that changes portal JS/CSS
	// is picked up immediately instead of serving a stale script (which broke the
	// report builder: new HTML + old cached portal-reports.js = undefined functions).
	staticFS := http.StripPrefix("/static/", http.FileServer(http.Dir("web/static")))
	mux.Handle("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		staticFS.ServeHTTP(w, r)
	}))

	// Versioned SDK serving (#106): publishers embed /sdk/v<major>/adtech.js with
	// per-channel cache headers + a /sdk/version.json metadata + integrity. Falls
	// back to the plain /static/ URL for the ad-tag snippet if the SDK can't load
	// (the bytes are baked into the gateway image, so this is belt-and-suspenders).
	sdkTagSrc := "/static/adtech.js"
	if sdkAsset, err := sdkasset.Load("web/static/adtech.js"); err != nil {
		log.Error("sdk asset load failed; versioned /sdk/ routes disabled", "error", err)
	} else {
		mux.HandleFunc("/sdk/", sdkHandler(sdkAsset, log))
		sdkTagSrc = sdkAsset.MajorURL()
		log.Info("sdk versioned serving ready", "version", sdkAsset.Version, "major", sdkAsset.Major, "integrity", sdkAsset.Integrity)
	}

	// Templates — loaded once, re-parsed on every render in dev mode so
	// editing a .html file in the editor shows up on the next browser
	// reload without a Go rebuild. Dev mode is detected from whether a
	// JWT signing key is configured (same heuristic the existing
	// "dev_mode" log line at the bottom of this function uses).
	templates, err := newTemplateManager(signingKey == "")
	if err != nil {
		log.Error("template load failed", "error", err)
		os.Exit(1)
	}

	// Dev tools (no auth - dev only). Routes go through the template
	// manager so the new `dict` FuncMap is available and later phases
	// can introduce shared layout/component partials without touching
	// this wiring.
	// Publisher simulator is one page (minimal.html, with Display +
	// Video tabs). The /minimal and trailing-slash URLs are kept as
	// aliases so existing bookmarks keep working. /video used to be a
	// separate template; removed once tabs landed in minimal.
	renderSim := func(w http.ResponseWriter, r *http.Request) { templates.Render(w, "minimal.html", nil) }
	mux.HandleFunc("/dev/publisher-simulator", renderSim)
	mux.HandleFunc("/dev/publisher-simulator/", renderSim)
	mux.HandleFunc("/dev/publisher-simulator/minimal", renderSim)

	// Simulator request-construction, single-sourced from pkg/simulator/request
	// so the web UI doesn't duplicate the consent/identity encoding in JS.
	mux.Handle(routes.SimRealism, middleware.CORS(http.HandlerFunc(simRealismHandler)))
	mux.Handle(routes.SimPersonas, middleware.CORS(http.HandlerFunc(simPersonasHandler)))
	mux.HandleFunc("/dev/trace-explorer", func(w http.ResponseWriter, r *http.Request) {
		templates.Render(w, "explorer.html", nil)
	})
	// Component-library showcase — the shared UI kit for the platform portals
	// (docs/UI_BUILD_PLAN.md, foundation F3). A living reference so new screens
	// compose from partials instead of copy-pasting markup.
	mux.HandleFunc("/dev/components", func(w http.ResponseWriter, r *http.Request) {
		templates.Render(w, "showcase.html", nil)
	})
	// Advertiser portal (UI plan Phase 1) — real screens on real APIs:
	// dashboard KPIs + spend trend (reports query), campaigns table with
	// create/pause/edit (DSP CRUD via /v1/api/campaigns), a report console,
	// and billing (balance + ledger-backed topup). Session claims drive the
	// nav filter and tenant scope; the dev bypass renders the admin view.
	// Served at /portal/advertiser; /dev/portal/advertiser kept as an alias.
	// Portal pages gate on a real session (redirect to /login) once auth is on;
	// in dev (no signing key) they pass through, same as the API bypass.
	advertiserPortal := requireLoginPage(signingKey, advertiserPortalHandler(templates, signingKey))
	mux.HandleFunc("/portal/advertiser", advertiserPortal)
	mux.HandleFunc("/dev/portal/advertiser", advertiserPortal)
	// Publisher portal (UI plan Phase 2) — same shape for the supply side:
	// dashboard (fill/eCPM/earnings), placements CRUD, ad-tag generator,
	// earnings/payouts.
	publisherPortal := requireLoginPage(signingKey, publisherPortalHandler(templates, signingKey))
	mux.HandleFunc("/portal/publisher", publisherPortal)
	mux.HandleFunc("/dev/portal/publisher", publisherPortal)
	// Staff console (UI plan Phase 3) — moderation queue, fraud blocklists,
	// audit-log viewer, plus links to the operator tools.
	staffPortal := requireLoginPage(signingKey, staffPortalHandler(templates, signingKey))
	mux.HandleFunc("/portal/staff", staffPortal)
	mux.HandleFunc("/dev/portal/staff", staffPortal)

	// Real auth: browser login → JWT stored in an httpOnly session cookie
	// (UI plan F4). Additive to the dev bypass — with no signing key set, the
	// Auth middleware still admin-bypasses; this path becomes the real gate once
	// gateway.require_auth + a jwt_signing secret are configured.
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		templates.Render(w, "login.html", nil)
	})
	mux.HandleFunc(routes.AuthLogin, loginSubmitHandler(dbUserLookup(gwDB), signingKey, log))
	mux.HandleFunc(routes.AuthLogout, logoutHandler)

	// Per-account OIDC SSO (PLAN Phase 11 #110): public start/callback (auth-code
	// flow); owner-only config CRUD behind auth. Password auth (above) is unaffected.
	var ssoStore ssoauth.Store
	if gwDB != nil {
		ssoStore = ssoauthpg.New(gwDB)
	}
	mux.HandleFunc(routes.AuthSSOStart, ssoStartHandler(ssoStore, signingKey, log))
	mux.HandleFunc(routes.AuthSSOCallback, ssoCallbackHandler(ssoStore, gwDB, signingKey, log))
	mux.Handle(routes.APIAccountSSO, authMiddleware(http.HandlerFunc(ssoConfigHandler(ssoStore, gwDB, log))))
	// Revoke-all-sessions is authenticated (you revoke your own sessions).
	mux.Handle(routes.AuthRevokeSessions, authMiddleware(http.HandlerFunc(revokeSessionsHandler(revStore, gwDB, log))))
	mux.HandleFunc("/signup", func(w http.ResponseWriter, r *http.Request) {
		templates.Render(w, "signup.html", nil)
	})
	mux.HandleFunc(routes.AuthSignup, signupHandler(pgSignupStore{db: gwDB}, signingKey, log))
	// /dev/landing/{brand} is the demo destination the tracker redirects
	// to after a click. Brand slug (luxauto, megastore, cryptoex, …) is
	// the last path segment; theme is picked from a small table so the
	// landing page visually matches the creative the user clicked. The
	// tracker appends ?adtech_tid=<trace_id> so this page can show the
	// trace + deep-link to the trace explorer / Jaeger.
	mux.HandleFunc("/dev/landing/", func(w http.ResponseWriter, r *http.Request) {
		slug := strings.TrimPrefix(r.URL.Path, "/dev/landing/")
		slug = strings.Trim(slug, "/")
		if slug == "" {
			slug = "default"
		}
		theme := landingThemeForSlug(slug)
		data := struct {
			Brand   string
			Theme   landingTheme
			TraceID string
			Slug    string
		}{
			Brand:   theme.Brand,
			Theme:   theme,
			TraceID: r.URL.Query().Get("adtech_tid"),
			Slug:    slug,
		}
		templates.Render(w, "brand.html", data)
	})
	if keys.Debug.EndpointsEnabled.Get(cfg) {
		// Reset+reseed for the pub sim. NATS publisher is opened lazily so
		// the cache-invalidate fan-out works even though the gateway has
		// no other reason to talk to NATS.
		resetBus, _ := natsbus.New(keys.NATS.URL.Get(cfg), constants.ServiceGateway, log)
		redisAddr := keys.Redis.URL.Get(cfg)
		// Reset-and-reseed is admin tooling (TRUNCATE + cross-tenant seed) the
		// least-privilege app role can't do — use the owner/admin URL when set,
		// else fall back to database.url (correct while the app is still owner).
		adminURL := cfg.Get(keys.Database.AdminURL.Key(), "")
		if adminURL == "" {
			adminURL = dbURL
		}
		mux.HandleFunc(routes.DevResetReseed, resetAndReseedHandler(adminURL, redisAddr, resetBus, log))
		// Clickable in-browser CONVERSION demo: the landing page (/dev/landing/{slug})
		// POSTs the click's trace id here; the gateway resolves the advertiser that
		// won the click, signs a /v1/t/conv postback with that advertiser's
		// conversion key, and fires it server-side (strict-mode safe). Mirrors
		// cmd/demoadv's /convert, but in-cluster so no second host process is needed.
		mux.HandleFunc(routes.DevLandingConvert, landingConvertHandler(reportingURL, trackerURL, log))
	}
	// /dev/console is the canonical command-center URL. /dev/config-manager
	// is an alias kept for backward compatibility — same template, just
	// historic naming. Both land on the tabbed console; the URL hash
	// (#config, #services, etc.) picks the active tab.
	// Legacy standalone console (config + secrets). Config editing now lives in
	// the staff portal (#config); this stays as a dev-only launcher and is gated
	// off in prod (debug.endpoints_enabled=false) — its /v1/config + /v1/services
	// data APIs are now authenticated regardless.
	if keys.Debug.EndpointsEnabled.Get(cfg) {
		consoleHandler := func(w http.ResponseWriter, r *http.Request) {
			templates.Render(w, "manager.html", nil)
		}
		mux.HandleFunc("/dev/console", consoleHandler)
		mux.HandleFunc("/dev/config-manager", consoleHandler)
	}

	// sellers.json (IAB standard - lists all active publishers we represent),
	// sourced live from the publishers table.
	mux.HandleFunc(routes.SellersJSON, sellersJSONHandler(pgSellerStore{db: gwDB}, log))

	// OpenAPI spec and Swagger UI
	mux.HandleFunc("/docs/openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		data, err := os.ReadFile("docs/openapi.yaml")
		if err != nil {
			http.Error(w, "spec not found", http.StatusNotFound)
			return
		}
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeYAML)
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Write(data)
	})
	mux.HandleFunc("/docs", swaggerUIHandler())
	mux.HandleFunc("/docs/", swaggerUIHandler())

	// Config management API — AUTHENTICATED. Reads leak DB URLs / internal
	// topology and writes mutate live config, so require a real session and the
	// config permission (read for GET, config:update for PUT/DELETE). The staff
	// Config UI calls these same-origin, so the session cookie flows.
	configAuth := func(h http.HandlerFunc) http.Handler {
		return authMiddleware(middleware.RequirePermissionByMethod(map[string]string{
			http.MethodGet:    "config:read",
			http.MethodPut:    "config:update",
			http.MethodDelete: "config:update",
		})(h))
	}
	mux.Handle(routes.Config, configAuth(cfgMgr.HTTPHandler()))
	mux.Handle(routes.ProxyConfig, configAuth(cfgMgr.HTTPHandler()))

	// Service registry - shows all running services and their config (topology
	// recon if open) — require an authenticated caller with config:read.
	// Registered UNCONDITIONALLY, registry resolved per request: when the
	// gateway boots during a Postgres race, sc.Registry is nil (MemorySource
	// path) and retryAttachPostgres attaches the real registry minutes later —
	// gating registration on the boot-time value latched the route out of
	// existence until a restart (unauth callers saw 404 where the security
	// posture promises 401; found by the security e2e after a VM restart).
	mux.Handle(routes.ServicesRegistry, authMiddleware(middleware.RequirePermission("config:read")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reg := sc.Manager.Registry()
		if reg == nil {
			http.Error(w, `{"error":"service registry warming (config store not yet attached)"}`, http.StatusServiceUnavailable)
			return
		}
		reg.HTTPHandler().ServeHTTP(w, r)
	}))))

	// Dev-only token minter: /v1/auth/token issues a signed JWT for an arbitrary
	// account/role with NO credential — a total auth bypass if reachable. Gated
	// behind debug.endpoints_enabled (which MUST be false in prod). Real callers
	// use /v1/auth/login (credentials) or /v1/auth/bootstrap (root password).
	if keys.Debug.EndpointsEnabled.Get(cfg) {
		mux.HandleFunc(routes.AuthToken, tokenHandler(signingKey))
	}
	// Bootstrap is the one-shot operator-key minting endpoint.
	// PLATFORM_ROOT_PASSWORD env var gates it; once a row with
	// name='bootstrap-admin-key' exists in secrets, subsequent calls
	// return 410. See cmd/gateway/bootstrap.go for the full design notes.
	mux.HandleFunc(routes.AuthBootstrap, bootstrapHandler(gwDB, log))

	// Secrets management API. Backed by the secrets warm cache for auth
	// (same gate as DSP/SSP CRUD); writes fan out via NATS so every
	// service's secrets cache refreshes sub-second. UI lives in the
	// Secrets sub-tab of /dev/console. (Cache started earlier, before the
	// auth middleware, so it can supply the JWT signing key.)
	hlth.AddReadinessCheck("secrets-cache", func(_ context.Context) error { return secretsCache.Ready() })
	secretsBus, _ := natsbus.New(keys.NATS.URL.Get(cfg), constants.ServiceGateway+"-secrets-mgmt", log)
	secretsAuth := middleware.AuthAPIKey(secretsCache, log)
	mux.Handle(routes.APISecrets, secretsAuth(http.HandlerFunc(secretsHandler(gwDB, secretsBus, log))))
	mux.Handle(routes.APISecrets+"/", secretsAuth(http.HandlerFunc(secretsHandler(gwDB, secretsBus, log))))

	// Audience upload endpoint (ADR 0007). Every upload is staged to the
	// onboarding bucket + recorded as one audience_ingest_jobs row; small,
	// due-now files run inline through the SAME pkg/ingest.Processor the pipeline
	// worker uses (200 + match rate), larger files are drained by that worker
	// (202 + job id, polled at .../ingest/{id}). gwDB may be nil if Postgres was
	// unreachable at boot; the handler 503s.
	var audStore *audiencepg.Store
	var ingestStore ingestjobs.Store
	var ingestProc *ingest.Processor
	onboardingBucket := keys.Pipeline.OnboardingBucket.Get(cfg)
	ingestObjects := objects.Connect(cfg, "/tmp/adtech-onboarding", log)
	if ingestObjects != nil {
		if err := ingestObjects.EnsureBucket(context.Background(), onboardingBucket); err != nil {
			log.Warn("audience ingest: ensure onboarding bucket failed", "bucket", onboardingBucket, "error", err)
		}
		// Ensure the PUBLIC-READ creatives bucket exists at boot. On a fresh object
		// store (e.g. after a PURGE teardown drops the Minio PVC) nothing else
		// creates it until a seed runs, and a host-run seed can't create buckets
		// over the localhost endpoint — so every creative asset 403s through the
		// /v1/creatives proxy and all ads render blank. Retry in the background
		// until the (self-healing) S3 backend is live + the bucket is public-read.
		go ensureCreativesBucket(ingestObjects, keys.S3.Bucket.Get(cfg), log)
	}
	if gwDB != nil {
		audStore = audiencepg.New(gwDB)
		ingestStore = ingestjobs.NewPostgresIngestStore(gwDB)
		ingestProc = &ingest.Processor{
			Objects:      ingestObjects,
			Audience:     audStore,
			Matcher:      postgres.NewFromDB(gwDB),
			Pipeline:     pipeline.New(log),
			Bus:          secretsBus,
			Log:          log,
			MaxRejectPct: keys.Pipeline.IngestMaxRejectPct.Get(cfg),
			Catalog:      catalogpg.New(gwDB),
		}
		// ADR 0008: load the platform PGP private key from the secrets cache so
		// providers can encrypt audience files to the platform public key. A
		// boot-time load — a rotation lands on the next gateway restart (the
		// active/rotating grace window keeps in-flight files decryptable). Absent
		// key = WARN + PGP files rejected as content failures; plaintext unaffected.
		if sec, ok := secretsCache.LookupActiveByPurpose(secrets.PurposePGPPrivate); ok && sec.Value != "" {
			if kr, err := pgp.ParsePrivate(sec.Value); err != nil {
				log.Error("audience ingest: parse pgp private key failed — PGP files will be rejected", "error", err)
			} else {
				ingestProc.PGPKeyring = kr
				log.Info("audience ingest: PGP decrypt-on-ingest enabled")
			}
		} else {
			log.Warn("audience ingest: no active pgp_private secret — PGP-encrypted uploads will be rejected")
		}
	}
	// ADR 0008: tenant-scoped custom field mappings ("connectors"). gwDB may be
	// nil at boot — the handlers 503 on a nil store, and an upload with no
	// mapping_id is unaffected.
	var mappingStore audiencemappings.Store
	if gwDB != nil {
		mappingStore = audiencemappings.NewPostgresStore(gwDB)
	}
	// ADR 0009: tenant-scoped data providers (the DMP entity). An upload that
	// selects a provider snapshots its party/licence/id_type/notify defaults.
	// nil store 503s the CRUD handler; an upload with no provider_id is unaffected.
	var providerStore dataproviders.Store
	if gwDB != nil {
		providerStore = dataproviders.NewPostgresStore(gwDB)
	}
	// Ingest completion emails (ADR 0008 Feature 3): inline uploads notify the
	// uploader (+ additional_emails) when a job finishes. SMTP when configured,
	// else an in-memory sender that only logs (dev).
	ingestEmailFrom := keys.Gateway.EmailFrom.Get(cfg)
	ingestEmailSender := connectIngestEmail(cfg, ingestEmailFrom, log)
	audDeps := audienceDeps{
		store:         audStore,
		proc:          ingestProc,
		ingestStore:   ingestStore,
		objects:       ingestObjects,
		bucket:        onboardingBucket,
		inlineMaxRow:  keys.Gateway.IngestInlineMaxRows.Get(cfg),
		maxBytes:      keys.Gateway.IngestMaxUploadBytes.Get(cfg),
		mappingStore:  mappingStore,
		providerStore: providerStore,
		db:            gwDB,
		emailSender:   ingestEmailSender,
		emailFrom:     ingestEmailFrom,
		log:           log,
	}
	// The ingest status subtree (.../ingest/{id}) must register BEFORE the base
	// path so {id} lookups aren't swallowed by the base handler.
	mux.Handle(routes.APIAudienceIngest, authMiddleware(http.HandlerFunc(audienceIngestStatusHandler(ingestStore, log))))
	// ADR 0008: the PGP public-key endpoint (more specific than the base
	// audiences path, so it isn't swallowed by it). JWT-gated tenant user.
	mux.Handle(routes.APIAudiencePGPKey, authMiddleware(audiencePGPKeyHandler(secretsCache, log)))
	// Phase I: the ads.txt authorisation policy for publishers, read from the
	// same live config the exchange enforces on. JWT-gated tenant user.
	mux.Handle(routes.APIIntegrationAdsTxt, authMiddleware(integrationAdsTxtHandler(cfg, log)))
	// ADR 0008: custom field mappings. ServeMux picks the LONGEST matching
	// pattern, so registration order is immaterial, but the intent is explicit:
	//   .../mappings/sample  (exact) — build a mapping from a sample upload
	//   .../mappings/        (subtree) — DELETE .../mappings/{id}
	//   .../mappings         (exact) — GET list / POST create-or-update
	// All three sit under /audiences but are more specific than the base
	// /audiences handler, so they aren't swallowed by it.
	mux.Handle(routes.APIAudienceMappingSample, authMiddleware(http.HandlerFunc(audienceMappingSampleHandler(keys.Gateway.IngestMaxUploadBytes.Get(cfg), log))))
	mux.Handle(routes.APIAudienceMappings+"/", authMiddleware(http.HandlerFunc(audienceMappingsHandler(mappingStore, log))))
	mux.Handle(routes.APIAudienceMappings, authMiddleware(http.HandlerFunc(audienceMappingsHandler(mappingStore, log))))
	// ADR 0009: data providers (the DMP entity). Base path (GET list / POST) +
	// subtree (.../{id} GET/DELETE); both more specific than the base /audiences.
	mux.Handle(routes.APIAudienceProviders+"/", authMiddleware(http.HandlerFunc(audienceProvidersHandler(providerStore, log))))
	mux.Handle(routes.APIAudienceProviders, authMiddleware(http.HandlerFunc(audienceProvidersHandler(providerStore, log))))
	mux.Handle(routes.APIAudiences, authMiddleware(http.HandlerFunc(audienceHandler(audDeps))))
	// Read-only audience inspection: member sample (per-row match report) +
	// the "test a hash" check. More specific than the base /audiences.
	mux.Handle(routes.APIAudienceMembers, authMiddleware(http.HandlerFunc(audienceInspectHandler(audDeps))))
	mux.Handle(routes.APIAudienceCheck, authMiddleware(http.HandlerFunc(audienceInspectHandler(audDeps))))
	mux.Handle(routes.APIAudienceSync, authMiddleware(http.HandlerFunc(audienceInspectHandler(audDeps))))
	// DPA slice 1: the advertiser product catalog (feed upload rides the same
	// ingest queue as audience files, kind=product; list reads the products table).
	var productCatalogStore *catalogpg.Store
	if gwDB != nil {
		productCatalogStore = catalogpg.New(gwDB)
	}
	mux.Handle(routes.APIProducts, authMiddleware(http.HandlerFunc(productsHandler(audDeps, productCatalogStore))))
	// Data marketplace (PLAN Phase 10): list/browse public segments for sale.
	var marketplaceStore *marketplacepg.Store
	if gwDB != nil {
		marketplaceStore = marketplacepg.New(gwDB)
	}
	mux.Handle(routes.APIMarketplaceListings, authMiddleware(http.HandlerFunc(marketplaceHandler(marketplaceStore, audStore, log))))
	// Per-listing actions (POST .../{id}/purchase | /estimate) + the caller's grants.
	mux.Handle(routes.APIMarketplaceListingsSub, authMiddleware(http.HandlerFunc(marketplaceListingActionHandler(marketplaceStore, audStore, gwDB, log))))
	mux.Handle(routes.APIMarketplaceGrants, authMiddleware(http.HandlerFunc(marketplaceGrantsHandler(marketplaceStore, log))))
	mux.Handle(routes.APIMarketplaceEarnings, authMiddleware(http.HandlerFunc(marketplaceEarningsHandler(marketplaceStore, log))))
	// Account closure + data export (PLAN Phase 11, item 105).
	closure := &closureDeps{db: gwDB}
	if gwDB != nil {
		closure.store = accountlifecyclepg.New(gwDB)
	}
	mux.Handle(routes.APIAccountClose, authMiddleware(http.HandlerFunc(accountClosureHandler(closure, log))))
	mux.Handle(routes.APIAccountCloseCancel, authMiddleware(http.HandlerFunc(accountCloseCancelHandler(closure, log))))
	// Support & dispute resolution (PLAN Phase 11, item 108).
	var supportStore support.Store
	if gwDB != nil {
		supportStore = supportpg.New(gwDB)
	}
	mux.Handle(routes.APISupportTickets, authMiddleware(http.HandlerFunc(supportTicketsHandler(supportStore, log))))
	mux.Handle(routes.APISupportTicketsSub, authMiddleware(http.HandlerFunc(supportTicketActionHandler(supportStore, gwDB, log))))
	mux.Handle(routes.APIAudienceRetargeting, authMiddleware(http.HandlerFunc(retargetingAudienceHandler(audDeps.store, log))))
	// IAB Audience Taxonomy: the global reference list for the portal picker +
	// the tenant-scoped label write. More specific paths than the base
	// /audiences handler, so neither is swallowed by it.
	mux.Handle(routes.APITaxonomy, authMiddleware(http.HandlerFunc(taxonomyHandler(audDeps.store, log))))
	mux.Handle(routes.APIAudienceTaxonomy, authMiddleware(http.HandlerFunc(audienceTaxonomyHandler(audDeps.store, secretsBus, log))))
	mux.Handle(routes.APIAudienceFee, authMiddleware(http.HandlerFunc(audienceFeeHandler(audDeps.store, secretsBus, log))))
	mux.Handle(routes.APIAudienceEarnings, authMiddleware(http.HandlerFunc(audienceEarningsHandler(audDeps.store, log))))

	// Advertiser conversion-event setup (define named conversions + embed pixel).
	// trackerURL is the browser-reachable tracker base baked into the pixel; a nil
	// store (Postgres down at boot) 503s.
	var convStore conversionStore
	if gwDB != nil {
		convStore = pgConversionStore{db: gwDB}
	}
	mux.Handle(routes.APIConversions, authMiddleware(http.HandlerFunc(conversionsHandler(convStore, publicTrackerURL, log))))

	// Advertiser self-serve HMAC key for signing S2S conversion postbacks (G7).
	// Tenant-scoped to the caller's account; the tracker validates /v1/t/conv
	// against this key by advid. Reuses the secrets NATS bus so a rotation
	// invalidates the tracker warm cache immediately.
	mux.Handle(routes.APIConversionKey, authMiddleware(http.HandlerFunc(conversionKeyHandler(gwDB, secretsBus, log))))

	// Identity-graph ingestion (link UID2 / hashed-email / device ids). gwDB may
	// be nil if Postgres was unreachable at boot; the handler 503s.
	var idStore identityLinkStore
	if gwDB != nil {
		idStore = postgres.NewFromDB(gwDB)
	}

	// Staff profile transparency + onboarding monitor (profile store payoff
	// valves) — platform-wide reads gated on support:read, audit-log posture.
	var profileResolver identityResolver
	if gwDB != nil {
		profileResolver = postgres.NewFromDB(gwDB)
	}
	mux.Handle(routes.APIProfiles+"/", authMiddleware(http.HandlerFunc(
		profilesHandler(gwDB, profileResolver, keys.Gateway.PipelineURL.Get(cfg), log))))
	mux.Handle(routes.APIOnboardingRuns, authMiddleware(http.HandlerFunc(onboardingMonitorHandler(gwDB, log))))
	mux.Handle(routes.APIStaffRetargeting, authMiddleware(http.HandlerFunc(staffRetargetingHandler(gwDB, log))))
	mux.Handle(routes.APIStaffChannels, authMiddleware(http.HandlerFunc(staffChannelsHandler(reportingURL, log))))
	// Shading insights — staff read-only view of the DSP's per-placement
	// win/loss shading state, proxied server-side (support:read).
	mux.Handle(routes.APIStaffShading, authMiddleware(http.HandlerFunc(staffShadingHandler(dspURL, log))))
	// Advertiser-facing bid-shading transparency (reports:read). Composes the
	// caller's OWN per-placement spend/clearing + durable win/loss, all from
	// ClickHouse (account-forced reporting queries: impressions for wins,
	// auction_losses for losses), with DSP-wide marketplace shading context for
	// those placements — labelled so the DSP-wide numbers are never presented as
	// the advertiser's own. The per-advertiser win/loss is now CH-sourced so it
	// survives a DSP redeploy (phase 2), not the DSP's in-memory tracker.
	mux.Handle(routes.APIShading, authMiddleware(
		middleware.RequirePermission("reports:read")(http.HandlerFunc(shadingHandler(shadingDeps{
			placementSpend:    reportingPlacementSpend(reportingURL, log),
			advertiserLosses:  reportingAdvertiserLosses(reportingURL, log),
			marketplaceStats:  dspMarketplaceStats(dspURL),
			advertiserSavings: reportingAdvertiserSavings(reportingURL, log),
		}, log)))))

	// Guided "Onboarding & Expansion" demo (staff-only, isolated synthetic
	// account). GET support:read (state), POST /run support:update (reset+run).
	// The exact route (/run) must register before the GET path so it doesn't
	// fall through to the GET handler.
	demoOrch := &demoOrchestrator{
		db: gwDB, aud: audStore, resolver: profileResolver, bus: secretsBus, log: log,
		// Same ingest deps as the real audience upload, so the demo's UPLOAD step
		// runs the real ingest path (and its profile_signal carries ingest_trace_id),
		// including the completion email to whoever runs the demo.
		proc: ingestProc, ingestStore: ingestStore, objects: ingestObjects, bucket: onboardingBucket,
		emailSender: ingestEmailSender, emailFrom: ingestEmailFrom,
	}
	demoOnboarding := authMiddleware(http.HandlerFunc(demoOnboardingHandler(demoOrch)))
	mux.Handle(routes.APIDemoOnboardingRun, demoOnboarding)
	mux.Handle(routes.APIDemoOnboarding, demoOnboarding)

	// Guided "Auction Trace" demo (staff-only): fires ONE fixed-persona ad
	// request through the real SSP serve path, captures its trace_id, polls the
	// real trace reader until the impression lands, and returns the 5-step
	// timeline. GET support:read (last-run snapshot), POST /run support:update
	// (fire+poll). The /run route registers before the GET path.
	traceDemoOrch := &traceDemoOrchestrator{
		backend: &httpTraceBackend{
			client:       &http.Client{Timeout: 10 * time.Second},
			sspURL:       sspURL,
			reportingURL: reportingURL,
			log:          log,
		},
		log: log,
	}
	demoTrace := authMiddleware(http.HandlerFunc(demoTraceHandler(traceDemoOrch)))
	mux.Handle(routes.APIDemoTraceRun, demoTrace)
	mux.Handle(routes.APIDemoTrace, demoTrace)

	// Guided "Billing / Money Flow" demo (staff-only): fires ONE fixed-persona
	// request so a real advertiser wins, snapshots that advertiser's prepay
	// balance + committed spend, fires the real impression beacon (the billable
	// event — spend books on the impression, not the win), polls Postgres until
	// the drawdown lands, and returns the 5-step before→after timeline. GET
	// support:read (last-run snapshot), POST /run support:update. /run first.
	billingDemoOrch := &billingDemoOrchestrator{
		backend: &httpBillingBackend{
			client: &http.Client{Timeout: 10 * time.Second},
			sspURL: sspURL,
			db:     gwDB,
			log:    log,
		},
		log: log,
	}
	demoBilling := authMiddleware(http.HandlerFunc(demoBillingHandler(billingDemoOrch)))
	mux.Handle(routes.APIDemoBillingRun, demoBilling)
	mux.Handle(routes.APIDemoBilling, demoBilling)

	// Guided "Rollups" demo (staff-only): fires N real fixed-persona impressions,
	// waits for them to land, then asks the reporting query API for the raw
	// impression row count and the same count grouped by the rollup dimensions —
	// one row per dimension-tuple (what a rollup row IS) — to show raw rows
	// collapsing into far fewer aggregated rows with identical totals. GET
	// support:read (last-run snapshot), POST /run support:update. /run first.
	rollupsDemoOrch := &rollupsDemoOrchestrator{
		backend: &httpRollupsBackend{
			client:       &http.Client{Timeout: 10 * time.Second},
			sspURL:       sspURL,
			reportingURL: reportingURL,
			log:          log,
		},
		log: log,
	}
	demoRollups := authMiddleware(http.HandlerFunc(demoRollupsHandler(rollupsDemoOrch)))
	mux.Handle(routes.APIDemoRollupsRun, demoRollups)
	mux.Handle(routes.APIDemoRollups, demoRollups)

	// Guided "Retargeting" demo (staff-only, isolated synthetic account) — the
	// behavioural/lake sibling of the onboarding demo. Fires the REAL /v1/t/rt
	// pixel + writes a site_visit behaviour_signals row to the SAME lake the
	// pipeline uses, then runs the REAL lake-backed profile-builder so its
	// behavioural rule matches the visit and cluster-expands the person into a
	// retargeting segment. The lake ObjectStore is built pure-Go (no duckdb
	// tag) from keys.S3.* + the datalake bucket; a nil lake (no store) 503s.
	// GET support:read (last-run snapshot), POST /run support:update. /run first.
	var rtBackend retargetingBackend
	if gwDB != nil {
		if lake := connectDatalake(context.Background(), cfg, log); lake != nil {
			rtBackend = &httpLakeRetargetingBackend{
				client:     &http.Client{Timeout: 10 * time.Second},
				trackerURL: trackerURL,
				db:         gwDB,
				lake:       lake,
				bus:        secretsBus,
				log:        log,
			}
		}
	}
	rtDemoOrch := &rtDemoOrchestrator{db: gwDB, aud: audStore, resolver: profileResolver, backend: rtBackend, log: log}
	demoRetargeting := authMiddleware(http.HandlerFunc(demoRetargetingHandler(rtDemoOrch)))
	mux.Handle(routes.APIDemoRetargetingRun, demoRetargeting)
	mux.Handle(routes.APIDemoRetargeting, demoRetargeting)

	mux.Handle(routes.APIBatchRuns, authMiddleware(http.HandlerFunc(batchMonitorHandler(gwDB, log))))
	mux.Handle(routes.APIBatchLake, authMiddleware(http.HandlerFunc(batchLakeHandler(reportingURL, log))))
	// Staff ad-tech glossary — static, code-grounded reference (pkg/glossary,
	// no DB). Read-only learning aid gated on support:read.
	mux.Handle(routes.APIGlossary, authMiddleware(http.HandlerFunc(glossaryHandler(log))))
	mux.Handle(routes.APIIdentityLinks, secretsAuth(http.HandlerFunc(identityLinksHandler(idStore, log))))

	// Privacy opt-out intake — operator-API-key auth like the others. Records
	// the opt-out + fans out so the DSP stops bidding for the user.
	mux.Handle(routes.APIPrivacyOptOut, secretsAuth(http.HandlerFunc(privacyOptOutHandler(gwDB, secretsBus, log))))
	// Staff privacy console — JWT-gated read surface over the same registry
	// (GET status?id= lookup / GET optouts recent list, both support:read) plus
	// the staff intake POST on behalf of a user (support:update), which
	// delegates to the SAME recorder the operator-key path uses so the two
	// intakes can't drift.
	mux.Handle(routes.APIPrivacyStatus, authMiddleware(http.HandlerFunc(privacyStatusHandler(pgPrivacyConsoleStore{db: gwDB}, log))))
	mux.Handle(routes.APIPrivacyOptOuts, authMiddleware(http.HandlerFunc(privacyOptOutsHandler(pgPrivacyConsoleStore{db: gwDB}, privacyOptOutHandler(gwDB, secretsBus, log), log))))

	// Team management — JWT-gated, tenant-scoped to the caller's account.
	mux.Handle(routes.APITeam, authMiddleware(http.HandlerFunc(teamHandler(pgTeamStore{db: gwDB}, log))))

	// Creative upload — POST to the exact (no-slash) path so it doesn't collide
	// with the GET list proxy at APICreatives (trailing slash → ad server).
	mux.Handle(strings.TrimSuffix(routes.APICreatives, "/"),
		authMiddleware(http.HandlerFunc(creativeUploadHandler(pgCreativeStore{db: gwDB}, log))))

	// Deals — JWT-gated, tenant-scoped; create verifies publisher ownership +
	// invalidates the exchange deal cache.
	mux.Handle(routes.APIDeals, authMiddleware(http.HandlerFunc(dealsHandler(pgDealStore{db: gwDB}, secretsBus, log))))
	mux.Handle(routes.APIDeals+"/", authMiddleware(http.HandlerFunc(dealByIDHandler(pgDealStore{db: gwDB}, secretsBus, log))))

	// Direct-sold line items — publisher's own commitments (sponsorship/
	// guaranteed/preferred/house), served by the publisher-adserver. JWT-gated
	// on deals:*, tenant-scoped; writes invalidate the publisher-line-items cache.
	mux.Handle(routes.APIDirectLineItems, authMiddleware(http.HandlerFunc(directLineItemsHandler(pgDirectLineItemStore{db: gwDB}, secretsBus, log))))
	mux.Handle(routes.APIDirectLineItems+"/", authMiddleware(http.HandlerFunc(directLineItemByIDHandler(pgDirectLineItemStore{db: gwDB}, secretsBus, log))))

	// Agency managed-accounts — staff assign advertiser accounts to an agency;
	// an agency session lists its own (drives the act-as switcher).
	mux.Handle(routes.APIAgencyAccounts, authMiddleware(http.HandlerFunc(agencyAccountsHandler(pgAgencyAccountStore{db: gwDB}, gwDB, log))))

	// Moderation — staff review queue (platform-wide, moderation:* gated).
	mux.Handle(routes.APIModeration, authMiddleware(http.HandlerFunc(moderationHandler(pgModerationStore{db: gwDB}, secretsBus, gwDB, log))))

	// Fraud blocklists — staff manager (platform-wide, fraud:* gated); mutations
	// invalidate the tracker fraud-rules warm cache.
	mux.Handle(routes.APIFraudBlocklists, authMiddleware(http.HandlerFunc(fraudRulesHandler(pgFraudRuleStore{db: gwDB}, secretsBus, gwDB, log))))

	// Webhooks — account subscription management (tenant-scoped, webhooks:*
	// gated); mutations invalidate the dispatcher's webhook-subs warm cache.
	mux.Handle(routes.APIWebhooks, authMiddleware(http.HandlerFunc(webhooksHandler(pgWebhookStore{db: gwDB}, secretsBus, log))))

	// In-app notifications — the portal bell feed (list + unread count, mark
	// read). Rows are written by cmd/notifications from NATS business events;
	// this is the read/mark-read side. Tenant-scoped; gwDB may be nil at boot
	// (handler 503s). Register the /read sub-path on the same handler.
	var notifStore notifications.Store
	if gwDB != nil {
		notifStore = notifications.NewPostgresStore(gwDB)
	}
	mux.Handle(routes.APINotifications, authMiddleware(http.HandlerFunc(notificationsHandler(notifStore, log))))
	mux.Handle(routes.APINotificationsRead, authMiddleware(http.HandlerFunc(notificationsHandler(notifStore, log))))

	// Saved reports — account saved/scheduled reports (tenant-scoped,
	// reports:read/reports:save gated).
	mux.Handle(routes.APISavedReports, authMiddleware(http.HandlerFunc(savedReportsHandler(pgSavedReportStore{db: gwDB}, log))))

	// Report jobs — the async report builder (submit/list on reports:read /
	// reports:export; status + gateway-streamed artifact download on the
	// subtree). The artifact bucket is private; this download path is the only
	// way an artifact leaves the platform.
	reportJobStore := newPGReportJobStore(gwDB)
	reportScope := reportjobs.PostgresScopeLookup{DB: gwDB}
	reportObjects := objects.Connect(cfg, "/tmp/adtech-reports", log)
	mux.Handle(routes.APIReportJobs, authMiddleware(http.HandlerFunc(reportJobsHandler(reportJobStore, reportScope, log))))
	mux.Handle(routes.APIReportJobs+"/", authMiddleware(http.HandlerFunc(reportJobByIDHandler(reportJobStore, reportObjects, log))))

	// Account data-export (PLAN Phase 11, item 105): status/enqueue + download
	// stream. Reuses the private reports bucket + object store.
	var exportStore accountexport.Store
	if gwDB != nil {
		exportStore = accountexportpg.New(gwDB)
	}
	exportH := accountExportHandler(exportStore, reportObjects, log)
	mux.Handle(routes.APIAccountExport, authMiddleware(exportH))
	mux.Handle(routes.APIAccountExport+"/", authMiddleware(exportH))

	// Payouts — publisher earnings/payout history (read-only, tenant-scoped,
	// earnings:view gated).
	mux.Handle(routes.APIPayouts, authMiddleware(http.HandlerFunc(payoutsHandler(pgPayoutStore{db: gwDB}, log))))
	mux.Handle(routes.APIMyRevshare, authMiddleware(http.HandlerFunc(myRevshareHandler(pgMyRevshareStore{db: gwDB}, log))))
	// Payout method — publisher payout destination + minimum-payout threshold
	// (GET reads earnings:view, PUT upserts on earnings:manage; raw details never
	// returned, tenant-scoped).
	mux.Handle(routes.APIPayoutMethod, authMiddleware(http.HandlerFunc(payoutMethodHandler(pgPayoutMethodStore{db: gwDB}, log))))

	// Quality controls — publisher allow/block lists (tenant-scoped, quality:*
	// gated); create verifies publisher ownership.
	mux.Handle(routes.APIQualityControls, authMiddleware(http.HandlerFunc(qualityControlsHandler(pgQualityControlStore{db: gwDB}, log))))

	// Ad-tag generator — publisher embed snippet per placement (read-only,
	// tenant-scoped, placements:read gated).
	mux.Handle(routes.APIAdTag, authMiddleware(http.HandlerFunc(adTagHandler(pgAdTagStore{db: gwDB}, sdkTagSrc, log))))

	// Topup — advertiser prepay credit (billing:view / billing:topup gated).
	// Money-touching: idempotency-keyed, double-entry ledger + balance in one
	// transaction; payment approval is the dev/fake path for now.
	mux.Handle(routes.APIBillingTopup, authMiddleware(http.HandlerFunc(topupHandler(pgTopupStore{db: gwDB}, secretsBus, log))))

	// Invoices — advertiser invoice history (read-only, tenant-scoped,
	// billing:view gated). List + detail share one handler; invoices are
	// written by the invoice-runner CronJob from billed committed spend.
	invoiceHandler := authMiddleware(http.HandlerFunc(invoicesHandler(pgInvoiceStore{db: gwDB}, log)))
	mux.Handle(routes.APIInvoices, invoiceHandler)
	mux.Handle(routes.APIInvoiceDetail, invoiceHandler)

	// Audit log — staff viewer over audit_log (read-only, audit:read gated,
	// platform-wide by design).
	mux.Handle(routes.APIAuditLog, authMiddleware(http.HandlerFunc(auditLogHandler(pgAuditLogStore{db: gwDB}, log))))

	// Accounts list — powers the staff impersonation picker (support:read,
	// platform-wide, read-only).
	mux.Handle(routes.APIAccounts, authMiddleware(middleware.RequirePermission("support:read")(accountsListHandler(gwDB, log))))
	mux.Handle(routes.APIAccountResidency, authMiddleware(middleware.RequirePermission("support:update")(setAccountResidencyHandler(gwDB, gwDB, log))))

	// Revshare — staff editor for publisher revenue-share splits (support:read
	// list / support:update edit); invalidates the billing-rates cache.
	mux.Handle(routes.APIRevshare, authMiddleware(http.HandlerFunc(revshareHandler(pgRevshareStore{db: gwDB}, secretsBus, log))))

	// Billing terms — staff editor for advertiser prepay/invoiced posture +
	// credit_limit (GET read on support:read, PUT set on support:update, both
	// enforced inside the handler). Invoiced accounts bid on credit up to the
	// limit; the PUT invalidates the DSP balance cache so the gate re-reads the
	// new terms within NATS RTT.
	mux.Handle(routes.APIBillingTerms, authMiddleware(http.HandlerFunc(billingTermsHandler(pgBillingTermsStore{db: gwDB}, secretsBus, log))))

	// House ads — staff editor for the platform's own fallback creatives served
	// on a no-bid (support:read list / support:update mutate). Platform-global
	// (no tenant scope); every mutation is audited + publishes the house-ads
	// cache invalidate so the publisher ad server reloads sub-second. The
	// master on/off (APIHouseAdsFill) sets the global stub_on_nobid config AND
	// clears per-pod overrides so the switch actually takes effect platform-wide.
	houseAdStore := newPGHouseAdStore(gwDB)
	mux.Handle(routes.APIHouseAdsFill, authMiddleware(http.HandlerFunc(houseAdsFillHandler(pgFillConfig{src: config.NewPostgresSource(gwDB)}, gwDB, secretsBus, keys.PublisherAdServer.StubOnNobid.Key(), log))))
	mux.Handle(routes.APIHouseAds, authMiddleware(http.HandlerFunc(houseAdsHandler(houseAdStore, secretsBus, log))))
	mux.Handle(routes.APIHouseAds+"/", authMiddleware(http.HandlerFunc(houseAdByIDHandler(houseAdStore, secretsBus, log))))

	// Staff ops console (/v1/api/ops/*) — monitor + act on the k8s stack from
	// the staff portal. The kubeops client only exists in-cluster; off-cluster
	// (bare `go run`) the k8s-backed handlers 503 with an ERROR log. Reads are
	// gated ops:read, mutations ops:deploy + an audit entry per action.
	kubeClient, kubeErr := kubeops.New()
	if kubeErr != nil {
		log.Warn("ops console: kubernetes client unavailable at boot (off-cluster?); /v1/api/ops k8s endpoints will 503", "error", kubeErr)
	}
	opsTargets := []opsTarget{
		{Name: constants.ServiceDSP, URL: dspURL},
		{Name: constants.ServiceSSP, URL: sspURL},
		{Name: constants.ServiceExchange, URL: exchangeURL},
		{Name: constants.ServiceTracker, URL: trackerURL},
		{Name: constants.ServiceAdServer, URL: adserverURL},
		{Name: constants.ServiceReporting, URL: reportingURL},
		{Name: constants.ServicePipeline, URL: keys.Gateway.PipelineURL.Get(cfg)},
		{Name: constants.ServicePublisherAdServer, URL: pubadURL},
		{Name: constants.ServiceSSAI, URL: keys.Gateway.SSAIURL.Get(cfg)},
	}
	opsAudit := func(ctx context.Context, e audit.Entry) {
		if err := audit.Log(ctx, gwDB, e); err != nil {
			log.Error("ops: audit write failed", "action", e.Action, "resource", e.ResourceID, "error", err)
		}
	}
	ops := newOpsAPI(kubeClient, opsTargets, natsMonitorURLFrom(keys.NATS.URL.Get(cfg)), opsAudit, log)
	opsRead := func(h http.HandlerFunc) http.Handler {
		return authMiddleware(middleware.RequirePermission("ops:read")(h))
	}
	opsDeploy := func(h http.HandlerFunc) http.Handler {
		return authMiddleware(middleware.RequirePermission("ops:deploy")(h))
	}
	mux.Handle(routes.APIOpsPods, opsRead(ops.podsHandler))
	mux.Handle(routes.APIOpsReadyzGrid, opsRead(ops.readyzGridHandler))
	mux.Handle(routes.APIOpsNATS, opsRead(ops.natsHandler))
	mux.Handle(routes.APIOpsCronJobs, opsRead(ops.cronJobsHandler))
	mux.Handle(routes.APIOpsJobs, opsRead(ops.jobsHandler))
	mux.Handle(routes.APIOpsPVCs, opsRead(ops.pvcsHandler))
	mux.Handle(routes.APIOpsLogs, opsRead(ops.logsHandler))
	mux.Handle(routes.APIOpsRestart, opsDeploy(ops.restartHandler))
	mux.Handle(routes.APIOpsCronJobTrigger, opsDeploy(ops.cronJobTriggerHandler))

	// Public status page (PLAN Phase 11, item 107): unauthed /status HTML +
	// /v1/api/status JSON, backed by readyz probes of the same ops targets and
	// staff-authored incidents. Incident CRUD is staff-only (incidents:*).
	statusServiceURLs := map[string]string{constants.ServiceGateway: ""}
	for _, t := range opsTargets {
		statusServiceURLs[t.Name] = t.URL
	}
	var incidentStore statuspage.Store
	if gwDB != nil {
		incidentStore = statuspagepg.New(gwDB)
	}
	statusAgg := newStatusAggregator(statusServiceURLs, incidentStore, templates, log)
	mux.HandleFunc(routes.StatusPage, statusAgg.pageHandler)
	mux.HandleFunc(routes.APIStatus, statusAgg.jsonHandler)
	mux.Handle(routes.APIIncidents, authMiddleware(http.HandlerFunc(incidentsHandler(incidentStore, gwDB, log))))

	// Public API changelog (PLAN Phase 11, item 109): unauthed /changelog HTML +
	// /v1/api/changelog JSON feed; staff CRUD at /v1/api/changelog/entries.
	var changelogStore changelog.Store
	if gwDB != nil {
		changelogStore = changelogpg.New(gwDB)
	}
	clog := &changelogServer{store: changelogStore, templates: templates, log: log}
	mux.HandleFunc(routes.Changelog, clog.pageHandler)
	mux.HandleFunc(routes.APIChangelog, clog.jsonHandler)
	mux.Handle(routes.APIChangelogEntries, authMiddleware(http.HandlerFunc(clog.entriesHandler(gwDB))))

	// External-partner onboarding registry (PLAN Phase 11, item 112) — staff-only
	// (partners:read/manage). Platform-global, like incidents.
	var partnerStore partner.Store
	if gwDB != nil {
		partnerStore = partnerpg.New(gwDB)
	}
	mux.Handle(routes.APIPartners, authMiddleware(http.HandlerFunc(partnersHandler(partnerStore, gwDB, log))))
	mux.Handle(routes.APIPartnerStatus, authMiddleware(http.HandlerFunc(partnerStatusHandler(partnerStore, gwDB, log))))
	mux.Handle(routes.APIPartnerProvision, authMiddleware(http.HandlerFunc(partnerProvisionHandler(partnerStore, gwDB, log))))
	mux.Handle(routes.APIPartnerMe, authMiddleware(http.HandlerFunc(partnerMeHandler(partnerStore, log))))
	mux.Handle(routes.APIPartnerSandboxKeys, authMiddleware(http.HandlerFunc(partnerSandboxKeysHandler(gwDB, secretsBus, log))))
	mux.Handle(routes.APIPartnerSandboxKeysRevoke, authMiddleware(http.HandlerFunc(partnerSandboxKeyRevokeHandler(gwDB, secretsBus, log))))
	mux.Handle(routes.APIPartnerValidate, authMiddleware(http.HandlerFunc(partnerValidateHandler(log))))
	mux.Handle(routes.APIPartnerTestBid, authMiddleware(http.HandlerFunc(partnerTestBidHandler(partnerStore, log))))
	mux.Handle(routes.APIPartnerCertify, authMiddleware(http.HandlerFunc(partnerCertifyHandler(partnerStore, log))))

	// Partner self-serve portal (PLAN Phase 11 #112 slice 2).
	partnerPortal := requireLoginPage(signingKey, partnerPortalHandler(templates, signingKey))
	mux.HandleFunc("/portal/partner", partnerPortal)
	mux.HandleFunc("/dev/portal/partner", partnerPortal)

	// Cache refresh — exposes the secrets warm cache so e2e tests and
	// ops can force a reload after rotation without waiting for the
	// 30s natural poll. Same shape as every other service's debug
	// endpoint (routes.DebugCacheRefresh).
	if keys.Debug.EndpointsEnabled.Get(cfg) {
		mux.HandleFunc(routes.DebugCacheRefresh, warm.RefreshHandler(secretsCache.Cache))
	}

	// API routes (auth required) - proxy to internal services
	//
	// The gateway is the auth boundary: browsers present a session (JWT
	// cookie), while the internal management APIs (DSP campaigns, SSP
	// placements) trust service API keys from the secrets store. The proxy
	// translates one into the other by injecting the gateway's service key
	// on the upstream request — without it every proxied call 401s at the
	// internal service. Dev default is the seeded dev key.
	serviceAPIKey := keys.Gateway.ServiceAPIKey.Get(cfg)
	withServiceKey := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Header.Set("X-API-Key", serviceAPIKey)
			next.ServeHTTP(w, r)
		})
	}

	// Campaigns: method-aware gate (reads campaigns:read, writes their
	// specific action). routes.APICampaigns carries a trailing slash, so it
	// is the {id} subtree; the TrimSuffix registration serves the exact
	// collection path without ServeMux's add-a-slash redirect (which turns
	// a PATCH into a lossy 307 round-trip).
	campaignsBase := strings.TrimSuffix(routes.APICampaigns, "/")
	campaignsProxy := middleware.RequirePermissionByMethod(map[string]string{
		http.MethodGet:    "campaigns:read",
		http.MethodPost:   "campaigns:create",
		http.MethodPatch:  "campaigns:update",
		http.MethodDelete: "campaigns:delete",
	})(withServiceKey(middleware.StripPrefix(campaignsBase, middleware.ReverseProxy(dspURL+routes.DSPCampaigns, log))))
	mux.Handle(campaignsBase, authMiddleware(campaignsProxy))
	mux.Handle(routes.APICampaigns, authMiddleware(campaignsProxy))

	// Placements: same exact+subtree registration as campaigns, with
	// method-aware permissions (PATCH/DELETE /v1/api/placements/{id} reaches
	// the SSP's by-ID handler; ownership is checked SSP-side via the
	// forwarded identity).
	placementsBase := strings.TrimSuffix(routes.APIPlacements, "/")
	placementsProxy := middleware.RequirePermissionByMethod(map[string]string{
		http.MethodGet:    "placements:read",
		http.MethodPost:   "placements:create",
		http.MethodPatch:  "placements:update",
		http.MethodDelete: "placements:delete",
	})(withServiceKey(middleware.StripPrefix(placementsBase, middleware.ReverseProxy(sspURL+routes.SSPPlacements, log))))
	mux.Handle(placementsBase, authMiddleware(placementsProxy))
	mux.Handle(routes.APIPlacements, authMiddleware(placementsProxy))

	// Publishers — GET lists via the SSP proxy (tenant-scoped SSP-side);
	// POST creates the caller's site locally (the onboarding step signup
	// doesn't cover). One route, method-dispatched.
	publishersList := middleware.RequirePermission("placements:read")(withServiceKey(
		middleware.StripPrefix(routes.APIPublishers, middleware.ReverseProxy(sspURL+routes.SSPPublishers, log))))
	publishersCreate := publisherCreateHandler(pgPublisherCreateStore{db: gwDB}, secretsBus, log)
	mux.Handle(routes.APIPublishers, authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			publishersCreate(w, r)
			return
		}
		publishersList.ServeHTTP(w, r)
	})))

	mux.Handle(routes.APICreatives, authMiddleware(
		middleware.RequirePermission("creatives:read")(withServiceKey(
			middleware.StripPrefix(routes.APICreatives, middleware.ReverseProxy(adserverURL+routes.AdCreatives, log))))))

	// Reports: exact + subtree registrations for the same reason as
	// campaigns — the portal POSTs to the bare path and a ServeMux
	// slash-redirect would cost every query an extra round trip.
	// enforceReportTenant rewrites the query body so customer sessions can
	// only query their own slice — the browser's filters are convenience,
	// this is the guarantee.
	reportsBase := strings.TrimSuffix(routes.APIReports, "/")
	reportsProxy := middleware.RequirePermission("reports:read")(
		enforceReportTenant(pgPublisherLookup{db: gwDB}, log)(
			middleware.StripPrefix(reportsBase, middleware.ReverseProxy(reportingURL+routes.ReportingQuery, log))))
	// Report-builder schema (tables + valid metrics/dimensions) — static, backend
	// source of truth for the portal dropdowns. More specific than the APIReports
	// catch-all, so it wins the mux match. reports:read; no tenant data.
	mux.Handle(routes.APIReportsSchema, authMiddleware(
		middleware.RequirePermission("reports:read")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			_ = json.NewEncoder(w).Encode(map[string]any{"tables": reporting.Schema()})
		}))))
	mux.Handle(reportsBase, authMiddleware(reportsProxy))
	mux.Handle(routes.APIReports, authMiddleware(reportsProxy))
	// Privacy Sandbox ARA reporting-only overlay (advertiser-scoped, reports:read).
	mux.Handle(routes.APIARAReports, authMiddleware(
		middleware.RequirePermission("reports:read")(araReportsHandler(gwDB, log))))

	// Trace inspector: GET proxies to reporting, which scopes + redacts per the
	// X-Account-Type/-ID the auth middleware injects. Staff (unscoped) and
	// advertiser (account_id) work now; publisher requires a validated
	// ?publisher_id (added with the publisher portal wiring) and safely 403s here
	// until then. reports:read is the right read permission for both.
	tracePubs := pgPublisherLookup{db: gwDB}
	traceProxy := middleware.RequirePermission("reports:read")(
		injectTraceScope(tracePubs, log)(
			middleware.StripPrefix(routes.APITrace, middleware.ReverseProxy(reportingURL+routes.ReportingTrace, log))))
	mux.Handle(routes.APITrace, authMiddleware(traceProxy))
	recentProxy := middleware.RequirePermission("reports:read")(
		injectTraceScope(tracePubs, log)(
			middleware.StripPrefix(routes.APIRecentImpressions, middleware.ReverseProxy(reportingURL+routes.ReportingRecentImpressions, log))))
	mux.Handle(routes.APIRecentImpressions, authMiddleware(recentProxy))

	// Attribution / multi-touch view: same scope-injection as the trace inspector;
	// reporting enforces the account filter from the injected X-Account-* headers.
	attrProxy := middleware.RequirePermission("reports:read")(
		injectTraceScope(tracePubs, log)(
			middleware.StripPrefix(routes.APIAttribution, middleware.ReverseProxy(reportingURL+routes.ReportingAttribution, log))))
	mux.Handle(routes.APIAttribution, authMiddleware(attrProxy))

	// Pass-through proxies (Swagger try-it-out, dev tools)
	mux.Handle(routes.ProxyReporting, middleware.CORS(middleware.ReverseProxy(reportingURL, log)))
	mux.Handle(routes.ProxyOpenRTB, middleware.CORS(middleware.ReverseProxy(exchangeURL, log)))
	mux.Handle(routes.ProxyTracker, middleware.CORS(middleware.ReverseProxy(trackerURL, log)))
	// Browser-side proxy for image creative assets (Minio / S3). The
	// ad server emits creatives.asset_url pointing at this path so
	// pixel HTML like <img src="https://gateway/v1/creatives/key.svg">
	// works from any browser without exposing the object store
	// directly. middleware.StripPrefix removes the /v1/creatives/
	// prefix so the object key reaches the backend unchanged.
	mux.Handle(routes.ProxyCreatives,
		middleware.CORS(middleware.StripPrefix(routes.ProxyCreatives,
			middleware.ReverseProxy(creativesStoreURL, log))))
	// Media proxy for the video sim's sample MP4s. Forwards
	// /v1/media/* → test-videos.co.uk so the MediaFile URL is
	// same-origin as the sim page, eliminating CORS as a variable
	// when IMA SDK fetches the MP4. Range requests + content-type
	// pass through unchanged.
	mux.Handle(routes.MediaProxy,
		middleware.CORS(middleware.StripPrefix(routes.MediaProxy,
			middleware.ReverseProxy("https://test-videos.co.uk/vids/", log))))
	mux.Handle(routes.ProxyAdServer, middleware.CORS(middleware.ReverseProxy(adserverURL, log)))
	mux.Handle(routes.ProxySSP, middleware.CORS(middleware.ReverseProxy(sspURL, log)))
	mux.Handle(routes.ProxyDSP, middleware.CORS(middleware.ReverseProxy(dspURL, log)))
	mux.Handle(routes.ProxyPubAd, middleware.CORS(middleware.ReverseProxy(pubadURL, log)))
	mux.Handle(routes.ProxySSAI, middleware.CORS(middleware.ReverseProxy(keys.Gateway.SSAIURL.Get(cfg), log)))
	mux.Handle(routes.ProxyBilling, middleware.CORS(middleware.ReverseProxy(reportingURL, log)))
	// Jaeger query API (browser → gateway → jaeger; Jaeger v1.58 has no CORS
	// on the query endpoint, so the pub sim reads spans through here).
	mux.Handle(routes.ProxyJaeger, middleware.CORS(middleware.StripPrefix(strings.TrimSuffix(routes.ProxyJaeger, "/"), middleware.ReverseProxy(jaegerURL, log))))

	// Dashboard home (no auth in dev mode). HTML lives in
	// web/templates/dashboard.html — see Phase 0 of the design audit
	// (docs/UI_DESIGN_AUDIT.md). dashboardHandler() is kept as a no-op
	// placeholder for callers/tests that may still reference it.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		// Root is not a page — send the visitor where they actually want to go:
		// a live session goes straight to its role's portal (dashboard); everyone
		// else goes to the login form. (The old dev-tools landing lives at
		// /dev-tools for anyone who still wants it.)
		if claims, ok := middleware.ParseSession(r, signingKey); ok {
			http.Redirect(w, r, portalHome(claims.AccountType), http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})

	// The old landing page (Publisher Simulator / Trace Explorer / Operator
	// Console / Swagger shortcuts) kept at an explicit path for discoverability.
	mux.HandleFunc("/dev-tools", func(w http.ResponseWriter, r *http.Request) {
		templates.Render(w, "dashboard.html", nil)
	})

	// Per-IP rate limit across the whole gateway (API + portals + login;
	// ratelimit_rps=0 → disabled). Guards login brute-force and API abuse.
	gwRL := middleware.NewLiveRateLimiter(func() middleware.RateLimitConfig {
		return middleware.RateLimitConfig{
			RPS:         keys.Gateway.RateLimitRPS.Get(cfg),
			Burst:       keys.Gateway.RateLimitBurst.Get(cfg),
			TrustedHops: keys.Gateway.RateLimitTrustedHops.Get(cfg),
			Allowlist:   keys.Gateway.RateLimitAllowlist.Get(cfg),
			Distributed: keys.Gateway.RateLimitDistributed.Get(cfg),
		}
	}, log).WithDistributedBackend(constants.ServiceGateway, gwL2)
	// SecurityHeaders is OUTERMOST so HSTS/X-Frame-Options/etc. ride every
	// response — including rate-limit 429s and error pages. CSRF sits just inside
	// it: it blocks cross-site cookie-authed state changes (defense-in-depth on
	// SameSite=Lax); Bearer/no-cookie/safe requests pass through untouched.
	// StripClientIdentityHeaders is OUTERMOST-but-one so no inbound request can
	// smuggle a gateway-trusted identity header (X-Account-*, X-User-ID,
	// X-Publisher-ID, X-Act-As-Account) past the edge. The legitimate setters
	// (ReverseProxy's JWT-claims block, injectTraceScope's validated publisher id)
	// run inside the mux, after this strip, so authenticated flows are unaffected;
	// the unauthenticated pass-through proxies (/v1/reporting/, /v1/billing/, …)
	// then forward NO identity header, so downstream scoping denies rather than
	// trusting a forged one.
	handler := middleware.SecurityHeaders(middleware.StripClientIdentityHeaders(middleware.CSRF(tracing.HTTPMiddleware(constants.ServiceGateway)(metrics.Wrap(gwRL.Wrap(mux))))))

	server := &http.Server{
		Addr:         ":" + port,
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	// Parallel HTTPS listener — runs only if both TLS_CERT_FILE and
	// TLS_KEY_FILE point at readable files. Required so Google's IMA
	// SDK iframe (which mirrors the host page's protocol) loads in a
	// secure context — Chrome refuses Private Network Access fetches
	// from non-secure contexts to loopback addresses regardless of any
	// CORS header. Same handler / routes; only the transport differs.
	// Best-effort: missing certs log a warning and skip the listener
	// rather than aborting boot, so prod and CI environments without
	// mounted certs continue to behave exactly as before.
	if cert, key := os.Getenv("TLS_CERT_FILE"), os.Getenv("TLS_KEY_FILE"); cert != "" && key != "" {
		if _, errC := os.Stat(cert); errC != nil {
			log.Warn("tls cert file not readable, https listener disabled", "path", cert, "error", errC)
		} else if _, errK := os.Stat(key); errK != nil {
			log.Warn("tls key file not readable, https listener disabled", "path", key, "error", errK)
		} else {
			tlsPort := os.Getenv("TLS_PORT")
			if tlsPort == "" {
				tlsPort = "8443"
			}
			tlsSrv := &http.Server{
				Addr:         ":" + tlsPort,
				Handler:      handler,
				ReadTimeout:  10 * time.Second,
				WriteTimeout: 30 * time.Second,
			}
			go func() {
				log.Info("gateway https listener starting", "port", tlsPort, "cert", cert)
				if err := tlsSrv.ListenAndServeTLS(cert, key); err != nil && err != http.ErrServerClosed {
					log.Error("https listener failed", "error", err)
				}
			}()
		}
	}

	devMode := "enabled"
	if signingKey != "" {
		devMode = "disabled"
	}
	log.Info("gateway starting", "port", port, "dev_mode", devMode)
	lifecycle.ServeHTTP(lc, server, log, 30*time.Second)
}

// tokenHandler issues JWT tokens. In dev mode (no signing key), returns a pre-built admin token.
// In production, this would validate credentials against Postgres.
func tokenHandler(signingKey string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			AccountID   string `json:"account_id"`
			AccountType string `json:"account_type"`
			Role        string `json:"role"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}

		if req.AccountID == "" {
			req.AccountID = "dev-account"
		}
		if req.AccountType == "" {
			req.AccountType = "admin"
		}
		if req.Role == "" {
			req.Role = "owner"
		}

		accountType := auth.AccountType(req.AccountType)
		role := auth.Role(req.Role)
		permissions := auth.RolePermissions(accountType, role)

		claims := &auth.Claims{
			UserID:      auth.MintUserID(req.AccountID),
			AccountID:   req.AccountID,
			AccountType: accountType,
			Role:        role,
			Permissions: permissions,
			IssuedAt:    time.Now(),
			ExpiresAt:   time.Now().Add(24 * time.Hour),
		}

		key := signingKey
		if key == "" {
			key = "dev-key" // dev mode fallback
		}

		token, err := middleware.CreateToken(claims, key)
		if err != nil {
			http.Error(w, "failed to create token", http.StatusInternalServerError)
			return
		}

		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"token":        token,
			"expires_at":   claims.ExpiresAt,
			"account_id":   claims.AccountID,
			"account_type": claims.AccountType,
			"role":         claims.Role,
			"permissions":  claims.Permissions,
		})
	}
}

func swaggerUIHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeHTML)
		w.Write([]byte(`<!DOCTYPE html>
<html>
<head>
  <title>Ad Tech API Docs</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
  <style>
    body { margin: 0; }
    .topbar { display: none; }
    .swagger-ui .info { margin: 20px 0; }
  </style>
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
  <script>
    SwaggerUIBundle({
      url: "/docs/openapi.yaml",
      dom_id: '#swagger-ui',
      presets: [SwaggerUIBundle.presets.apis],
      layout: "BaseLayout",
      docExpansion: "list",
      filter: true,
      tagsSorter: function(a, b) {
        // Custom sort: Customer first, then Partner, then Internal, then Dev
        var order = {"Auth": 0, "[Customer]": 1, "[Partner]": 2, "[Internal]": 3, "[Dev]": 4};
        var aKey = Object.keys(order).find(function(k) { return a.indexOf(k) >= 0; }) || "[Dev]";
        var bKey = Object.keys(order).find(function(k) { return b.indexOf(k) >= 0; }) || "[Dev]";
        return (order[aKey] || 5) - (order[bKey] || 5);
      }
    });
  </script>
</body>
</html>`))
	}
}

// ensureCreativesBucket creates the public-read creatives bucket on a fresh/racing
// object store and keeps retrying until the S3 backend is live (the fs fallback
// has no public-read, so SetPublicRead errors until the real S3 swaps in). Bounded
// so a never-arriving Minio doesn't leak a goroutine forever; logs once on success.
func ensureCreativesBucket(store objects.Store, bucket string, log *slog.Logger) {
	type publicReader interface {
		SetPublicRead(context.Context, string) error
	}
	for attempt := 1; attempt <= 60; attempt++ {
		errEnsure := store.EnsureBucket(context.Background(), bucket)
		var errPublic error = errEnsure
		if errEnsure == nil {
			if pr, ok := store.(publicReader); ok {
				errPublic = pr.SetPublicRead(context.Background(), bucket)
			}
		}
		if errEnsure == nil && errPublic == nil {
			if attempt > 1 {
				log.Info("creatives bucket ensured + public-read", "bucket", bucket, "attempt", attempt)
			}
			return
		}
		log.Warn("ensuring creatives bucket (will retry)", "bucket", bucket, "attempt", attempt, "ensure_err", errEnsure, "public_err", errPublic)
		time.Sleep(15 * time.Second)
	}
	log.Error("gave up ensuring creatives bucket public-read; creative assets may 403", "bucket", bucket)
}
