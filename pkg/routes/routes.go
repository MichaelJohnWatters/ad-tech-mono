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

	// Docs
	DocsSwagger = "/docs"
	DocsOpenAPI = "/docs/openapi.yaml"
	SellersJSON = "/sellers.json"

	// Dev tools
	DevPublisherSim = "/dev/publisher-simulator"
	DevTraceExplorer = "/dev/trace-explorer"

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
	SSPRequest    = "/v1/ssp/request"
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
