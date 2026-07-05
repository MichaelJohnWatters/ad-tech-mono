// cmd/gateway is the API Gateway and dashboard server.
// Single entry point for all REST/dashboard traffic.
// Handles auth (JWT/RBAC), proxies API calls to internal services,
// and serves the HTMX dashboard.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"

	audiencepg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache/warm"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events/natsbus"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/fraud"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/health"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/secrets"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
	_ "github.com/lib/pq"
)

func main() {
	log := logger.New(constants.ServiceGateway)
	sc := config.Setup(constants.ServiceGateway, gatewaySchema, log)
	cfg := sc.Cfg
	cfgMgr := sc.Manager
	hlth := health.New()
	lc := lifecycle.New(log)

	// The config-manager UI reads the union of every running pod's schema
	// from service_registry.schema_entries — no separate "load published
	// schema" step is needed here.
	dbURL := cfg.Get("database.url", "")

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
			}
		} else {
			log.Warn("gateway db open failed; bootstrap endpoint will return 503", "error", err)
		}
	}

	// OpenTelemetry — empty endpoint disables tracing so dev/test envs
	// without Jaeger still boot. Same pattern as the auction-path services.
	otelShutdown := tracing.Init(context.Background(), tracing.Config{
		ServiceName:    constants.ServiceGateway,
		ServiceVersion: cfg.Get("otel.service_version", "dev"),
		Endpoint:       cfg.Get("otel.endpoint", "localhost:4318"),
		SampleRatio:    cfg.GetFloat("otel.sample_ratio", 1.0),
		Log:            log,
	})
	lc.OnShutdown("otel", func(ctx context.Context) error { return otelShutdown(ctx) })

	port := cfg.Get("gateway.port", routes.PortGateway)

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
	signingKey := cfg.Get("gateway.jwt_signing_key", "")
	if sec, ok := secretsCache.LookupActiveByPurpose(secrets.PurposeJWTSigning); ok {
		signingKey = sec.Value
		log.Info("jwt signing key loaded from secrets store", "name", sec.Name)
	}
	if signingKey == "" {
		if cfg.GetBool("gateway.require_auth", false) {
			log.Error("gateway.require_auth=true but no JWT signing key is available (no active jwt_signing secret, empty gateway.jwt_signing_key); refusing to boot with auth bypassed")
			os.Exit(1)
		}
		log.Warn("SECURITY: no JWT signing key configured — auth is BYPASSED, every request receives admin claims. Dev only; set a jwt_signing secret (or gateway.require_auth=true) in staging/prod.")
	}

	// Internal service URLs (configurable for staging/prod)
	dspURL := cfg.Get("gateway.dsp_url", routes.DefaultDSPURL)
	sspURL := cfg.Get("gateway.ssp_url", routes.DefaultSSPURL)
	adserverURL := cfg.Get("gateway.adserver_url", routes.DefaultAdServerURL)
	pubadURL := cfg.Get("gateway.publisher_adserver_url", routes.DefaultPublisherAdServerURL)
	reportingURL := cfg.Get("gateway.reporting_url", routes.DefaultReportingURL)
	exchangeURL := cfg.Get("gateway.exchange_url", routes.DefaultExchangeURL)
	trackerURL := cfg.Get("gateway.tracker_url", routes.DefaultTrackerURL)
	jaegerURL := cfg.Get("gateway.jaeger_url", routes.DefaultJaegerURL)
	// Object-store forwarding target for /v1/creatives/* — the
	// browser-reachable proxy for SVG / PNG / JPG assets stored in
	// Minio or S3. Default points at the in-cluster Minio service;
	// staging/prod overlays override with the real S3 endpoint.
	creativesStoreURL := cfg.Get("gateway.creatives_store_url", "http://"+routes.DefaultMinioEndpoint+"/"+cfg.Get("s3.bucket", "adtech-creatives")+"/")

	authMiddleware := middleware.Auth(signingKey, log)

	metrics := middleware.NewMetrics(constants.ServiceGateway)

	mux := http.NewServeMux()

	// Health (no auth)
	mux.Handle(routes.Healthz, hlth.LivenessHandler())
	mux.Handle(routes.Readyz, hlth.ReadinessHandler())
	mux.Handle(routes.Metrics, metrics.Handler())

	// Static files (no auth)
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("web/static"))))

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
	mux.HandleFunc("/v1/auth/login", loginSubmitHandler(dbUserLookup(gwDB), signingKey, log))
	mux.HandleFunc("/v1/auth/logout", logoutHandler)
	mux.HandleFunc("/signup", func(w http.ResponseWriter, r *http.Request) {
		templates.Render(w, "signup.html", nil)
	})
	mux.HandleFunc("/v1/auth/signup", signupHandler(pgSignupStore{db: gwDB}, signingKey, log))
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
		}{
			Brand:   theme.Brand,
			Theme:   theme,
			TraceID: r.URL.Query().Get("adtech_tid"),
		}
		templates.Render(w, "brand.html", data)
	})
	if cfg.GetBool("debug.endpoints_enabled", true) {
		// Reset+reseed for the pub sim. NATS publisher is opened lazily so
		// the cache-invalidate fan-out works even though the gateway has
		// no other reason to talk to NATS.
		resetBus, _ := natsbus.New(cfg.Get("nats.url", routes.DefaultNATSURL), constants.ServiceGateway, log)
		redisAddr := cfg.Get("redis.url", routes.DefaultRedisAddr)
		mux.HandleFunc(routes.DevResetReseed, resetAndReseedHandler(dbURL, redisAddr, resetBus, log))
	}
	// /dev/console is the canonical command-center URL. /dev/config-manager
	// is an alias kept for backward compatibility — same template, just
	// historic naming. Both land on the tabbed console; the URL hash
	// (#config, #services, etc.) picks the active tab.
	consoleHandler := func(w http.ResponseWriter, r *http.Request) {
		templates.Render(w, "manager.html", nil)
	}
	mux.HandleFunc("/dev/console", consoleHandler)
	mux.HandleFunc("/dev/config-manager", consoleHandler)

	// sellers.json (IAB standard - lists all publishers we represent)
	mux.HandleFunc("/sellers.json", func(w http.ResponseWriter, r *http.Request) {
		sellers := fraud.GenerateSellersJSON([]fraud.SellerEntry{
			{SellerID: "pub-daily-news", Name: "Daily News", Domain: "daily-news.com", SellerType: "PUBLISHER"},
			{SellerID: "pub-tech-review", Name: "Tech Review", Domain: "tech-review.io", SellerType: "PUBLISHER"},
			{SellerID: "pub-sports-daily", Name: "Sports Daily", Domain: "sports-daily.com", SellerType: "PUBLISHER"},
			{SellerID: "pub-shoppers-hub", Name: "Shoppers Hub", Domain: "shoppers-hub.com", SellerType: "PUBLISHER"},
		})
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		json.NewEncoder(w).Encode(sellers)
	})

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

	// Config management API
	mux.HandleFunc(routes.Config, cfgMgr.HTTPHandler())
	mux.HandleFunc(routes.ProxyConfig, cfgMgr.HTTPHandler())

	// Service registry - shows all running services and their config
	if sc.Registry != nil {
		mux.HandleFunc("/v1/services", sc.Registry.HTTPHandler())
	}

	// Auth endpoint
	mux.HandleFunc(routes.AuthToken, tokenHandler(signingKey))
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
	secretsBus, _ := natsbus.New(cfg.Get("nats.url", routes.DefaultNATSURL), constants.ServiceGateway+"-secrets-mgmt", log)
	secretsAuth := middleware.AuthAPIKey(secretsCache, log)
	mux.Handle(routes.APISecrets, secretsAuth(http.HandlerFunc(secretsHandler(gwDB, secretsBus, log))))
	mux.Handle(routes.APISecrets+"/", secretsAuth(http.HandlerFunc(secretsHandler(gwDB, secretsBus, log))))

	// Audience CRM-upload endpoint — same operator-API-key auth as secrets.
	// gwDB may be nil if Postgres was unreachable at boot; the handler 503s.
	var audStore *audiencepg.Store
	if gwDB != nil {
		audStore = audiencepg.New(gwDB)
	}
	mux.Handle(routes.APIAudiences, secretsAuth(http.HandlerFunc(audienceHandler(audStore, secretsBus, log))))

	// Privacy opt-out intake — operator-API-key auth like the others. Records
	// the opt-out + fans out so the DSP stops bidding for the user.
	mux.Handle(routes.APIPrivacyOptOut, secretsAuth(http.HandlerFunc(privacyOptOutHandler(gwDB, secretsBus, log))))

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

	// Moderation — staff review queue (platform-wide, moderation:* gated).
	mux.Handle(routes.APIModeration, authMiddleware(http.HandlerFunc(moderationHandler(pgModerationStore{db: gwDB}, secretsBus, log))))

	// Fraud blocklists — staff manager (platform-wide, fraud:* gated); mutations
	// invalidate the tracker fraud-rules warm cache.
	mux.Handle(routes.APIFraudBlocklists, authMiddleware(http.HandlerFunc(fraudRulesHandler(pgFraudRuleStore{db: gwDB}, secretsBus, log))))

	// Webhooks — account subscription management (tenant-scoped, webhooks:*
	// gated); mutations invalidate the dispatcher's webhook-subs warm cache.
	mux.Handle(routes.APIWebhooks, authMiddleware(http.HandlerFunc(webhooksHandler(pgWebhookStore{db: gwDB}, secretsBus, log))))

	// Saved reports — account saved/scheduled reports (tenant-scoped,
	// reports:read/reports:save gated).
	mux.Handle(routes.APISavedReports, authMiddleware(http.HandlerFunc(savedReportsHandler(pgSavedReportStore{db: gwDB}, log))))

	// Payouts — publisher earnings/payout history (read-only, tenant-scoped,
	// earnings:view gated).
	mux.Handle(routes.APIPayouts, authMiddleware(http.HandlerFunc(payoutsHandler(pgPayoutStore{db: gwDB}, log))))

	// Quality controls — publisher allow/block lists (tenant-scoped, quality:*
	// gated); create verifies publisher ownership.
	mux.Handle(routes.APIQualityControls, authMiddleware(http.HandlerFunc(qualityControlsHandler(pgQualityControlStore{db: gwDB}, log))))

	// Ad-tag generator — publisher embed snippet per placement (read-only,
	// tenant-scoped, placements:read gated).
	mux.Handle(routes.APIAdTag, authMiddleware(http.HandlerFunc(adTagHandler(pgAdTagStore{db: gwDB}, log))))

	// Topup — advertiser prepay credit (billing:view / billing:topup gated).
	// Money-touching: idempotency-keyed, double-entry ledger + balance in one
	// transaction; payment approval is the dev/fake path for now.
	mux.Handle(routes.APIBillingTopup, authMiddleware(http.HandlerFunc(topupHandler(pgTopupStore{db: gwDB}, secretsBus, log))))

	// Audit log — staff viewer over audit_log (read-only, audit:read gated,
	// platform-wide by design).
	mux.Handle(routes.APIAuditLog, authMiddleware(http.HandlerFunc(auditLogHandler(pgAuditLogStore{db: gwDB}, log))))

	// Revshare — staff editor for publisher revenue-share splits (support:read
	// list / support:update edit); invalidates the billing-rates cache.
	mux.Handle(routes.APIRevshare, authMiddleware(http.HandlerFunc(revshareHandler(pgRevshareStore{db: gwDB}, secretsBus, log))))

	// Cache refresh — exposes the secrets warm cache so e2e tests and
	// ops can force a reload after rotation without waiting for the
	// 30s natural poll. Same shape as every other service's debug
	// endpoint (routes.DebugCacheRefresh).
	if cfg.GetBool("debug.endpoints_enabled", true) {
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
	serviceAPIKey := cfg.Get("gateway.service_api_key", "dev-api-key-do-not-use-in-prod")
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
	mux.Handle(reportsBase, authMiddleware(reportsProxy))
	mux.Handle(routes.APIReports, authMiddleware(reportsProxy))

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
	mux.Handle("/v1/media/",
		middleware.CORS(middleware.StripPrefix("/v1/media/",
			middleware.ReverseProxy("https://test-videos.co.uk/vids/", log))))
	mux.Handle(routes.ProxyAdServer, middleware.CORS(middleware.ReverseProxy(adserverURL, log)))
	mux.Handle(routes.ProxySSP, middleware.CORS(middleware.ReverseProxy(sspURL, log)))
	mux.Handle(routes.ProxyDSP, middleware.CORS(middleware.ReverseProxy(dspURL, log)))
	mux.Handle(routes.ProxyPubAd, middleware.CORS(middleware.ReverseProxy(pubadURL, log)))
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
		templates.Render(w, "dashboard.html", nil)
	})

	handler := tracing.HTTPMiddleware(constants.ServiceGateway)(metrics.Wrap(mux))

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
			UserID:      "user-" + req.AccountID,
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
