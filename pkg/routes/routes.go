// Package routes defines all HTTP route paths for the platform.
// Every service imports route constants from here instead of hardcoding strings.
// Grouped by service so you can see the full API surface in one place.
package routes

// API version prefix. When we move to v2, add new constants
// and keep v1 for backwards compatibility.
const APIVersion = "v1"
const apiPrefix = "/" + APIVersion

// ============================================================
// Gateway (:8080) - public entry point
// ============================================================

const (
	// Auth
	AuthToken = "/v1/auth/token"

	// Config management
	Config        = "/v1/config"
	ConfigHistory = "/v1/config/history"

	// API proxy routes (gateway -> internal services)
	APICampaigns  = apiPrefix + "/api/campaigns/"
	APIPlacements = apiPrefix + "/api/placements/"
	APICreatives  = apiPrefix + "/api/creatives/"
	APIReports    = apiPrefix + "/api/reports/"

	// Pass-through proxy prefixes (gateway -> internal, for Swagger try-it-out)
	ProxyReporting = apiPrefix + "/reporting/"
	ProxyOpenRTB   = apiPrefix + "/openrtb/"
	ProxyTracker   = apiPrefix + "/t/"
	ProxyAdServer  = apiPrefix + "/ad/"
	ProxySSP       = apiPrefix + "/ssp/"
	ProxyDSP       = apiPrefix + "/dsp/"
	ProxyBilling   = apiPrefix + "/billing/"
	ProxyConfig    = apiPrefix + "/config/"
	// ProxyJaeger forwards browser fetches to the Jaeger query API. Used by
	// the pub sim to read trace spans post-hoc. Goes through the gateway
	// because Jaeger v1.58 doesn't support CORS on the query endpoint.
	ProxyJaeger = apiPrefix + "/jaeger/"

	// Docs
	DocsSwagger = "/docs"
	DocsOpenAPI = "/docs/openapi.yaml"
	SellersJSON = "/sellers.json"

	// Dev tools
	DevPublisherSim = "/dev/publisher-simulator"
	DevTraceExplorer = "/dev/trace-explorer"
	// DevResetReseed wipes all tenant data and re-runs the seed profile.
	// Gated by debug.endpoints_enabled. Used by the pub sim's "Reset &
	// reseed" button so you don't need to drop to a terminal between runs.
	DevResetReseed = "/dev/reset-and-reseed"

	// Health
	Healthz = "/healthz"
	Readyz  = "/readyz"
	Metrics = "/metrics"
)

// ============================================================
// Exchange (:8081) - auction pipeline
// ============================================================

const (
	OpenRTBAuction = "/v1/openrtb/auction"
	OpenRTBWin     = "/v1/openrtb/win"
	OpenRTBLoss    = "/v1/openrtb/loss"
)

// ============================================================
// DSP (:8082) - bid evaluation
// ============================================================

const (
	OpenRTBBid    = "/v1/openrtb/bid"
	DSPCampaigns  = "/v1/dsp/campaigns"
	DSPShading    = "/v1/dsp/shading"
)

// ============================================================
// Tracker (:8083) - event pixels
// ============================================================

const (
	TrackerImpression = "/v1/t/imp"
	TrackerClick      = "/v1/t/click"
	TrackerConversion = "/v1/t/conv"
	TrackerView       = "/v1/t/view"
	TrackerVideo      = "/v1/t/video"
	TrackerAudio      = "/v1/t/audio"
)

// ============================================================
// SSP (:8084) - publisher inventory
// ============================================================

const (
	SSPPlacements = "/v1/ssp/placements"
	// SSPPublishers returns the publishers a placement can be attached to.
	// Used by the publisher simulator's "+ New Placement" modal as the
	// dropdown source.
	SSPPublishers = "/v1/ssp/publishers"
	// SSPRequest returns the raw OpenRTB BidResponse — used by e2e tests
	// and the (deprecated) X-ray dev path. Real publisher pages don't hit
	// this; auction details (winner, clearing price, fan-out) should never
	// leak to the browser.
	SSPRequest = "/v1/ssp/request"
	// SSPServe is the realistic publisher-visitor path: SSP runs the
	// auction internally, calls the ad server, and returns just the
	// rendered HTML + pixel URLs. The browser never sees winner/pricing.
	// Used by the publisher simulator and what a real adtech.js SDK would
	// call on a publisher page.
	SSPServe = "/v1/ssp/serve"
)

// ============================================================
// Ad Server (:8085) - creative serving
// ============================================================

const (
	AdServe     = "/v1/ad/serve"
	AdCreatives = "/v1/ad/creatives"
	AdBandit    = "/v1/ad/bandit"
)

// ============================================================
// Reporting (:8086) - analytics + billing
// ============================================================

const (
	ReportingQuery  = "/v1/reporting/query"
	ReportingEvents = "/v1/reporting/events"
	BillingSummary  = "/v1/billing/summary"
	BillingLedger   = "/v1/billing/ledger"
)

// ============================================================
// Debug endpoints (all services)
// ============================================================
//
// Behind debug.endpoints_enabled config (default true in dev). Exposed by
// any service that holds warm caches so tests and ops tooling can force a
// synchronous reload from Postgres without waiting for the poll interval.
const (
	DebugCacheRefresh = "/debug/cache/refresh"
	// DebugAuctionWins returns the count of auction-win records the reporting
	// in-memory analytics store has for a given trace_id. Used by e2e tests
	// to verify adtech.auction.win NATS events reached reporting exactly once.
	DebugAuctionWins = "/debug/auction_wins"
)

// ============================================================
// Service base URLs (default local dev)
// ============================================================

// DefaultHost is the base hostname for local dev. Override via config for staging/prod.
const DefaultHost = "localhost"

const (
	DefaultGatewayURL   = "http://" + DefaultHost + ":" + PortGateway
	DefaultExchangeURL  = "http://" + DefaultHost + ":" + PortExchange
	DefaultDSPURL       = "http://" + DefaultHost + ":" + PortDSP
	DefaultTrackerURL   = "http://" + DefaultHost + ":" + PortTracker
	DefaultSSPURL       = "http://" + DefaultHost + ":" + PortSSP
	DefaultAdServerURL  = "http://" + DefaultHost + ":" + PortAdServer
	DefaultReportingURL = "http://" + DefaultHost + ":" + PortReporting
	DefaultPipelineURL  = "http://" + DefaultHost + ":" + PortPipeline
	DefaultNATSURL      = "nats://" + DefaultHost + ":" + PortNATSClient
	DefaultDSPComp1URL  = "http://" + DefaultHost + ":" + PortDSPComp1
	DefaultDSPComp2URL  = "http://" + DefaultHost + ":" + PortDSPComp2
	DefaultJaegerURL    = "http://" + DefaultHost + ":" + PortJaeger
)

// ServiceURL builds a URL from host and port.
func ServiceURL(host, port string) string {
	return "http://" + host + ":" + port
}

// NATSURL builds a NATS connection URL from host and port.
func NATSURL(host, port string) string {
	return "nats://" + host + ":" + port
}

// ============================================================
// Ports
// ============================================================

const (
	PortGateway     = "8080"
	PortExchange    = "8081"
	PortDSP         = "8082"
	PortDSPComp1    = "8089"
	PortDSPComp2    = "8090"
	PortTracker     = "8083"
	PortSSP         = "8084"
	PortAdServer    = "8085"
	PortReporting   = "8086"
	PortPipeline    = "8087"
	PortGrafana     = "3000"
	PortPrometheus  = "9090"
	PortJaeger      = "16686"
	PortNATSClient  = "4222"
	PortNATSMonitor = "8222"
	PortPostgres    = "5432"
	PortRedis       = "6379"
	PortMinioAPI    = "9000"
	PortMinioUI     = "9001"
)
