// cmd/gateway is the API Gateway and dashboard server.
// Single entry point for all REST/dashboard traffic.
// Handles auth (JWT/RBAC), proxies API calls to internal services,
// and serves the HTMX dashboard.
package main

import (
	"encoding/json"
	"net/http"
	"os"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/fraud"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/health"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

func main() {
	log := logger.New(constants.ServiceGateway)
	sc := config.Setup(constants.ServiceGateway, log)
	cfg := sc.Cfg
	cfgMgr := sc.Manager
	hlth := health.New()
	lc := lifecycle.New(log)

	port := cfg.Get("gateway.port", routes.PortGateway)
	signingKey := cfg.Get("gateway.jwt_signing_key", "")

	// Internal service URLs (configurable for staging/prod)
	dspURL := cfg.Get("gateway.dsp_url", routes.DefaultDSPURL)
	sspURL := cfg.Get("gateway.ssp_url", routes.DefaultSSPURL)
	adserverURL := cfg.Get("gateway.adserver_url", routes.DefaultAdServerURL)
	reportingURL := cfg.Get("gateway.reporting_url", routes.DefaultReportingURL)
	exchangeURL := cfg.Get("gateway.exchange_url", routes.DefaultExchangeURL)
	trackerURL := cfg.Get("gateway.tracker_url", routes.DefaultTrackerURL)

	authMiddleware := middleware.Auth(signingKey, log)

	mux := http.NewServeMux()

	// Health (no auth)
	mux.Handle(routes.Healthz, hlth.LivenessHandler())
	mux.Handle(routes.Readyz, hlth.ReadinessHandler())

	// Static files (no auth)
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("web/static"))))

	// Dev tools (no auth - dev only)
	mux.HandleFunc("/dev/publisher-simulator", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "web/templates/simulator/minimal.html")
	})
	mux.HandleFunc("/dev/publisher-simulator/minimal", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "web/templates/simulator/minimal.html")
	})
	mux.HandleFunc("/dev/trace-explorer", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "web/templates/trace/explorer.html")
	})
	mux.HandleFunc("/dev/config-manager", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "web/templates/config/manager.html")
	})

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

	// API routes (auth required) - proxy to internal services
	mux.Handle(routes.APICampaigns, authMiddleware(
		middleware.RequirePermission("campaigns:read")(
			middleware.StripPrefix(routes.APICampaigns, middleware.ReverseProxy(dspURL+routes.DSPCampaigns, log)))))

	mux.Handle(routes.APIPlacements, authMiddleware(
		middleware.RequirePermission("placements:read")(
			middleware.StripPrefix(routes.APIPlacements, middleware.ReverseProxy(sspURL+routes.SSPPlacements, log)))))

	mux.Handle(routes.APICreatives, authMiddleware(
		middleware.RequirePermission("creatives:read")(
			middleware.StripPrefix(routes.APICreatives, middleware.ReverseProxy(adserverURL+routes.AdCreatives, log)))))

	mux.Handle(routes.APIReports, authMiddleware(
		middleware.RequirePermission("reports:read")(
			middleware.StripPrefix(routes.APIReports, middleware.ReverseProxy(reportingURL+routes.ReportingQuery, log)))))

	// Pass-through proxies (Swagger try-it-out, dev tools)
	mux.Handle(routes.ProxyReporting, middleware.CORS(middleware.ReverseProxy(reportingURL, log)))
	mux.Handle(routes.ProxyOpenRTB, middleware.CORS(middleware.ReverseProxy(exchangeURL, log)))
	mux.Handle(routes.ProxyTracker, middleware.CORS(middleware.ReverseProxy(trackerURL, log)))
	mux.Handle(routes.ProxyAdServer, middleware.CORS(middleware.ReverseProxy(adserverURL, log)))
	mux.Handle(routes.ProxySSP, middleware.CORS(middleware.ReverseProxy(sspURL, log)))
	mux.Handle(routes.ProxyDSP, middleware.CORS(middleware.ReverseProxy(dspURL, log)))
	mux.Handle(routes.ProxyBilling, middleware.CORS(middleware.ReverseProxy(reportingURL, log)))

	// Dashboard home (no auth in dev mode)
	mux.HandleFunc("/", dashboardHandler())

	server := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
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

func dashboardHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeHTML)
		w.Write([]byte(`<!DOCTYPE html>
<html class="dark" lang="en">
<head>
<title>Ad Tech Platform</title>
<script src="https://cdn.tailwindcss.com"></script>
<script>
tailwind.config = {
  darkMode: 'class',
  theme: { extend: { colors: { brand: { DEFAULT: '#4361ee', hover: '#3a56d4' }, surface: { dark: '#1a1a2e', page: '#0f0f1a' } } } }
}
</script>
<script src="https://unpkg.com/htmx.org@2.0.4"></script>
<script>
(function(){var s=localStorage.getItem('theme');if(s)document.documentElement.className=s;else if(window.matchMedia('(prefers-color-scheme:light)').matches)document.documentElement.className='light'})();
function toggleTheme(){var h=document.documentElement,n=h.classList.contains('dark')?'light':'dark';h.className=n;localStorage.setItem('theme',n);document.getElementById('ti').textContent=n==='dark'?'\u2600\uFE0F':'\uD83C\uDF19'}
</script>
</head>
<body class="bg-gray-50 dark:bg-surface-page text-gray-900 dark:text-gray-200 min-h-screen">

<nav class="bg-white dark:bg-surface-dark border-b border-gray-200 dark:border-gray-800 px-6 py-3 flex items-center gap-4">
  <a href="/" class="font-semibold text-lg text-gray-900 dark:text-white">Ad Tech</a>
  <div class="flex gap-3 text-sm">
    <a href="/dev/publisher-simulator" class="text-gray-500 dark:text-gray-400 hover:text-brand">Simulator</a>
    <a href="/dev/trace-explorer" class="text-gray-500 dark:text-gray-400 hover:text-brand">Traces</a>
    <a href="/dev/config-manager" class="text-gray-500 dark:text-gray-400 hover:text-brand">Config</a>
    <a href="/docs" class="text-gray-500 dark:text-gray-400 hover:text-brand">API Docs</a>
  </div>
  <button onclick="toggleTheme()" id="ti" class="ml-auto text-lg px-2 py-1 rounded hover:bg-gray-100 dark:hover:bg-gray-800">☀️</button>
</nav>

<main class="max-w-2xl mx-auto px-5 py-8">

<div class="bg-emerald-50 dark:bg-emerald-950/30 border-l-4 border-emerald-400 dark:border-emerald-500 p-4 rounded-r-lg mb-6 text-sm">
  3 DSPs competing in real-time auctions. Full pipeline: SSP &rarr; Exchange &rarr; DSP &rarr; Ad Server &rarr; Tracker &rarr; Reporting.
</div>

<section class="mb-8">
  <h2 class="text-sm font-semibold uppercase tracking-wider text-gray-400 dark:text-gray-500 mb-3">Developer Tools</h2>
  <div class="grid gap-3">
    <a href="/dev/publisher-simulator" class="block p-4 rounded-lg bg-white dark:bg-surface-dark border border-gray-200 dark:border-gray-800 hover:border-brand transition-colors">
      <div class="font-medium text-brand">Publisher Simulator</div>
      <div class="text-sm text-gray-500 dark:text-gray-400">Simulated publisher page with real auctions, ad sizes, and debug overlay</div>
    </a>
    <a href="/dev/trace-explorer" class="block p-4 rounded-lg bg-white dark:bg-surface-dark border border-gray-200 dark:border-gray-800 hover:border-brand transition-colors">
      <div class="font-medium text-brand">Trace Explorer</div>
      <div class="text-sm text-gray-500 dark:text-gray-400">Trace a single ad request through every service end-to-end</div>
    </a>
    <a href="/dev/config-manager" class="block p-4 rounded-lg bg-white dark:bg-surface-dark border border-gray-200 dark:border-gray-800 hover:border-brand transition-colors">
      <div class="font-medium text-brand">Config Manager</div>
      <div class="text-sm text-gray-500 dark:text-gray-400">View and edit live config for all services, per-pod overrides, change history</div>
    </a>
    <a href="/docs" class="block p-4 rounded-lg bg-white dark:bg-surface-dark border border-gray-200 dark:border-gray-800 hover:border-brand transition-colors">
      <div class="font-medium text-brand">API Docs (Swagger)</div>
      <div class="text-sm text-gray-500 dark:text-gray-400">Full OpenAPI spec - Customer, Partner, and Internal endpoints with try-it-out</div>
    </a>
  </div>
</section>

<section class="mb-8">
  <h2 class="text-sm font-semibold uppercase tracking-wider text-gray-400 dark:text-gray-500 mb-3">Observability</h2>
  <div class="grid grid-cols-2 sm:grid-cols-3 gap-2">
    <a href="http://localhost:3000" target="_blank" class="block p-3 rounded-lg bg-white dark:bg-surface-dark border border-gray-200 dark:border-gray-800 hover:border-brand text-center text-sm transition-colors">
      <div class="font-medium">Grafana</div><div class="text-xs text-gray-400">:3000</div>
    </a>
    <a href="http://localhost:9090" target="_blank" class="block p-3 rounded-lg bg-white dark:bg-surface-dark border border-gray-200 dark:border-gray-800 hover:border-brand text-center text-sm transition-colors">
      <div class="font-medium">Prometheus</div><div class="text-xs text-gray-400">:9090</div>
    </a>
    <a href="http://localhost:16686" target="_blank" class="block p-3 rounded-lg bg-white dark:bg-surface-dark border border-gray-200 dark:border-gray-800 hover:border-brand text-center text-sm transition-colors">
      <div class="font-medium">Jaeger</div><div class="text-xs text-gray-400">:16686</div>
    </a>
    <a href="http://localhost:10350" target="_blank" class="block p-3 rounded-lg bg-white dark:bg-surface-dark border border-gray-200 dark:border-gray-800 hover:border-brand text-center text-sm transition-colors">
      <div class="font-medium">Tilt</div><div class="text-xs text-gray-400">:10350</div>
    </a>
    <a href="http://localhost:8222" target="_blank" class="block p-3 rounded-lg bg-white dark:bg-surface-dark border border-gray-200 dark:border-gray-800 hover:border-brand text-center text-sm transition-colors">
      <div class="font-medium">NATS</div><div class="text-xs text-gray-400">:8222</div>
    </a>
    <a href="http://localhost:9001" target="_blank" class="block p-3 rounded-lg bg-white dark:bg-surface-dark border border-gray-200 dark:border-gray-800 hover:border-brand text-center text-sm transition-colors">
      <div class="font-medium">Minio</div><div class="text-xs text-gray-400">:9001</div>
    </a>
  </div>
</section>

<section class="mb-8">
  <h2 class="text-sm font-semibold uppercase tracking-wider text-gray-400 dark:text-gray-500 mb-3">Services</h2>
  <div class="bg-white dark:bg-surface-dark rounded-lg border border-gray-200 dark:border-gray-800 divide-y divide-gray-100 dark:divide-gray-800 text-sm">
    <div class="px-4 py-2 flex justify-between"><span class="font-medium">Gateway</span><span class="text-gray-400 font-mono">:8080</span></div>
    <div class="px-4 py-2 flex justify-between"><span class="font-medium">Exchange</span><span class="text-gray-400 font-mono">:8081</span></div>
    <div class="px-4 py-2 flex justify-between"><span class="font-medium">DSP</span><span class="text-gray-400 font-mono">:8082 :8089 :8090</span></div>
    <div class="px-4 py-2 flex justify-between"><span class="font-medium">Tracker</span><span class="text-gray-400 font-mono">:8083</span></div>
    <div class="px-4 py-2 flex justify-between"><span class="font-medium">SSP</span><span class="text-gray-400 font-mono">:8084</span></div>
    <div class="px-4 py-2 flex justify-between"><span class="font-medium">Ad Server</span><span class="text-gray-400 font-mono">:8085</span></div>
    <div class="px-4 py-2 flex justify-between"><span class="font-medium">Reporting</span><span class="text-gray-400 font-mono">:8086</span></div>
    <div class="px-4 py-2 flex justify-between"><span class="font-medium">Pipeline</span><span class="text-gray-400 font-mono">:8087</span></div>
  </div>
</section>

<section class="mb-8">
  <h2 class="text-sm font-semibold uppercase tracking-wider text-gray-400 dark:text-gray-500 mb-3">Debug</h2>
  <div class="flex flex-wrap gap-2 text-sm">
    <a href="/v1/dsp/campaigns" class="px-3 py-1.5 rounded bg-white dark:bg-surface-dark border border-gray-200 dark:border-gray-800 hover:border-brand transition-colors">DSP Campaigns</a>
    <a href="/v1/dsp/shading" class="px-3 py-1.5 rounded bg-white dark:bg-surface-dark border border-gray-200 dark:border-gray-800 hover:border-brand transition-colors">Win-Rate Data</a>
    <a href="/v1/ssp/placements" class="px-3 py-1.5 rounded bg-white dark:bg-surface-dark border border-gray-200 dark:border-gray-800 hover:border-brand transition-colors">SSP Placements</a>
    <a href="/v1/ad/creatives" class="px-3 py-1.5 rounded bg-white dark:bg-surface-dark border border-gray-200 dark:border-gray-800 hover:border-brand transition-colors">Creatives</a>
    <a href="/healthz" class="px-3 py-1.5 rounded bg-white dark:bg-surface-dark border border-gray-200 dark:border-gray-800 hover:border-brand transition-colors">/healthz</a>
    <a href="/readyz" class="px-3 py-1.5 rounded bg-white dark:bg-surface-dark border border-gray-200 dark:border-gray-800 hover:border-brand transition-colors">/readyz</a>
  </div>
</section>

<section>
  <h2 class="text-sm font-semibold uppercase tracking-wider text-gray-400 dark:text-gray-500 mb-3">Quick Test</h2>
  <pre class="bg-white dark:bg-surface-dark border border-gray-200 dark:border-gray-800 rounded-lg p-4 text-sm font-mono overflow-x-auto">go run ./cmd/simulator single --geo GBR --device mobile</pre>
</section>

</main>

<footer class="text-center py-6 text-xs text-gray-400 dark:text-gray-600">
  Ad Tech Platform &middot; <a href="/docs" class="text-brand">API Docs</a> &middot; <a href="http://localhost:10350" target="_blank" class="text-brand">Tilt</a>
</footer>
</body>
</html>`))
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
