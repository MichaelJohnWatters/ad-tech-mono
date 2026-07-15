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
	// AuthBootstrap is the one-shot endpoint for minting the first operator
	// API key after a fresh deploy. Gated by PLATFORM_ROOT_PASSWORD env var
	// (must match exactly); on success creates a row in the secrets table
	// and returns the new key value once. Self-disables: subsequent calls
	// return 410 Gone because the bootstrap row now exists. Operators
	// rotate this initial key via the secrets UI thereafter.
	AuthBootstrap = "/v1/auth/bootstrap"

	// Config management
	Config        = "/v1/config"
	ConfigHistory = "/v1/config/history"

	// API proxy routes (gateway -> internal services)
	APICampaigns  = apiPrefix + "/api/campaigns/"
	APIPlacements = apiPrefix + "/api/placements/"
	APICreatives  = apiPrefix + "/api/creatives/"
	APIReports    = apiPrefix + "/api/reports/"
	// APITrace / APIRecentImpressions back the portal trace inspector; the
	// gateway proxies them to reporting's ReportingTrace / RecentImpressions.
	APITrace             = apiPrefix + "/api/trace"
	APIRecentImpressions = apiPrefix + "/api/impressions/recent"

	// Gateway-local CRUD: secrets management (operator-only). Reads the
	// secrets table directly and publishes adtech.cache.invalidate.secrets
	// on every mutation so service warm caches re-sync sub-second. Gated
	// by middleware.AuthAPIKey, not the JWT proxy chain — the Secrets
	// tab in /dev/console drives this.
	APISecrets = apiPrefix + "/api/secrets"
	// APIAudiences is the CRM/audience upload endpoint (create segment +
	// bulk-add members). POST only.
	APIAudiences = apiPrefix + "/api/audiences"
	// APIIdentityLinks ingests identity-graph edges (link a UID2 token / hashed
	// email / device id to other identifiers). Operator-API-key auth.
	APIIdentityLinks = apiPrefix + "/api/identity-links"

	// APIPrivacyOptOut is the user opt-out intake (level 1/2/3). POST only.
	// Records opt_out_registry + publishes OptOutEvent + cache-invalidate.
	APIPrivacyOptOut = apiPrefix + "/api/privacy/optout"

	// APITeam manages the caller's account team members (GET list, POST invite).
	// JWT-gated; tenant-scoped to the caller's account.
	APITeam = apiPrefix + "/api/team"

	// APIDeals is publisher deal management (GET list, POST create). JWT-gated;
	// tenant-scoped; a create must reference a publisher the caller owns.
	APIDeals = apiPrefix + "/api/deals"

	// APIDirectLineItems is publisher direct-sold line-item management (GET
	// list, POST create; PATCH on the /{id} subtree). JWT-gated on deals:*;
	// tenant-scoped; served by the publisher-adserver arbitration ladder.
	APIDirectLineItems = apiPrefix + "/api/direct-line-items"

	// APIAgencyAccounts manages agency → managed-advertiser assignments. GET
	// lists (an agency sees its own, staff see all / by ?agency_id); POST
	// assigns and DELETE unassigns (staff only). Drives agency act-as.
	APIAgencyAccounts = apiPrefix + "/api/agency-accounts"

	// APIModeration is the staff creative-review queue (GET pending, POST
	// approve/reject). JWT-gated on moderation:* — platform-wide (not tenant
	// scoped): staff review every account's creatives.
	APIModeration = apiPrefix + "/api/moderation"

	// APIFraudBlocklists is the staff fraud blocklist manager (GET list, POST
	// add, DELETE remove). JWT-gated on fraud:* — platform-wide (fraud_blocklists
	// has no account_id): a create/delete publishes the fraud-rules cache
	// invalidate so the tracker warm cache reloads sub-second.
	APIFraudBlocklists = apiPrefix + "/api/fraud/blocklists"

	// APIWebhooks is account webhook-subscription management (GET list, POST
	// create, DELETE remove). JWT-gated on webhooks:*; tenant-scoped. Mutations
	// publish the webhook-subs cache invalidate so the dispatcher reloads.
	APIWebhooks = apiPrefix + "/api/webhooks"

	// APISavedReports is saved/scheduled report management (GET list, POST
	// create, DELETE remove). JWT-gated on reports:read/reports:save;
	// tenant-scoped.
	APISavedReports = apiPrefix + "/api/reports/saved"

	// APIReportJobs is the async report builder: POST submit a report job
	// (reports:export), GET list jobs (reports:read); the subtree serves
	// GET {id} status (reports:read) and GET {id}/download (reports:export,
	// streamed through the gateway — the artifact bucket is private).
	// Tenant-scoped.
	APIReportJobs = apiPrefix + "/api/reports/jobs"

	// APIPayouts is the publisher earnings/payout history (GET only). JWT-gated
	// on earnings:view; tenant-scoped; read-only (payouts are created by the
	// billing settlement job).
	APIPayouts = apiPrefix + "/api/payouts"

	// APIQualityControls is publisher inventory quality-control management (GET
	// list, POST upsert, DELETE remove). JWT-gated on quality:read/quality:update;
	// tenant-scoped; a create must reference a publisher the caller owns.
	APIQualityControls = apiPrefix + "/api/quality-controls"

	// APIAdTag generates a publisher embed snippet for a placement (GET only,
	// ?placement_id=&tag_type=js|prebid|vast). JWT-gated on placements:read;
	// tenant-scoped; read-only (derives the tag from the placement + pubad
	// routes).
	APIAdTag = apiPrefix + "/api/adtag"

	// APIBillingTopup is the advertiser prepay topup (GET balance+history on
	// billing:view, POST credit on billing:topup). Money-touching: POST is
	// idempotent (client-supplied idempotency_key) and writes a double-entry
	// ledger pair + balance upsert in one transaction. The payment leg is the
	// dev/fake instant-approve path until a real provider is integrated.
	APIBillingTopup = apiPrefix + "/api/billing/topup"

	// APIPublishers proxies to the SSP publishers list (GET only,
	// placements:read). Tenant-scoped SSP-side via the forwarded identity —
	// a publisher session gets only its own publishers; platform users all.
	APIPublishers = apiPrefix + "/api/publishers"

	// APIAccounts lists advertiser + publisher accounts for the staff
	// impersonation picker (GET only, support:read). Platform-wide, read-only.
	APIAccounts = apiPrefix + "/api/accounts"

	// APIAuditLog is the staff audit-log viewer (GET only, audit:read).
	// Platform-wide by design (operator tool) with exact-match filters:
	// ?account_id=&action=&resource_type=&resource_id=&limit=.
	APIAuditLog = apiPrefix + "/api/audit"

	// APIProfiles is the staff profile-transparency lookup (GET /{id},
	// support:read): identity cluster + direct graph links + memberships
	// with provenance + lake signal summary — "what do we know about this
	// user", trace-explorer style. Platform-wide by design (operator tool).
	APIProfiles = apiPrefix + "/api/profiles"

	// APIBatchRuns is the staff batch-conductor monitor (GET, support:read):
	// recent chain runs with per-step status — what ran, in what order,
	// what failed, what got skipped.
	APIBatchRuns = apiPrefix + "/api/batch/runs"

	// APIOnboardingRuns is the staff drop-zone monitor (GET, support:read):
	// recent onboarding_runs + per-provider rollups (files, rejects, match
	// rates). Platform-wide operational telemetry.
	APIOnboardingRuns = apiPrefix + "/api/onboarding/runs"

	// APIRevshare is the staff revenue-share editor (GET list on support:read,
	// PATCH ?id= on support:update). Platform-wide commercial term; updates
	// publishers.revshare_config and invalidates the billing-rates cache so
	// reporting's ContractLoader re-reads the split.
	APIRevshare = apiPrefix + "/api/revshare"
	// APIMyRevshare is the publisher-scoped read of the caller's OWN revenue-
	// share terms (GET, earnings:view) — so the publisher portal can show net
	// (post-fee) earnings and the fee split without the staff-only editor.
	APIMyRevshare = apiPrefix + "/api/my-revshare"

	// Pass-through proxy prefixes (gateway -> internal, for Swagger try-it-out)
	ProxyReporting = apiPrefix + "/reporting/"
	ProxyOpenRTB   = apiPrefix + "/openrtb/"
	ProxyTracker   = apiPrefix + "/t/"
	// ProxyCreatives forwards browser GETs for creative assets (SVG /
	// PNG / JPG in the S3/Minio bucket) to the gateway's configured
	// object-store endpoint. Lets creatives.asset_url point at a
	// browser-reachable URL even when the storage backend lives on
	// cluster-internal DNS the browser can't resolve. Same pattern as
	// ProxyTracker.
	ProxyCreatives = apiPrefix + "/creatives/"
	ProxyAdServer  = apiPrefix + "/ad/"
	ProxySSP       = apiPrefix + "/ssp/"
	ProxyDSP       = apiPrefix + "/dsp/"
	ProxyPubAd     = apiPrefix + "/pubad/"
	ProxySSAI      = apiPrefix + "/ssai/"
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
	DevPublisherSim  = "/dev/publisher-simulator"
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
// Pipeline (:8087) - data lake
// ============================================================

const (
	// DatalakePurge (POST {"user_id":...}) runs the GDPR filtered rewrite
	// over the user-keyed lake tables (profile_signals, behaviour_signals).
	// Lives on the pipeline because it is the lake's SINGLE WRITER — a purge
	// from any other process would race its version allocation.
	DatalakePurge = "/v1/datalake/purge"
	// DatalakeResidual (GET ?user_id=) counts rows still keyed to the user —
	// the privacy-verify residual check for the lake systems.
	DatalakeResidual = "/v1/datalake/residual"
	// DatalakeProfile (GET ?user_id=) returns the user's lake-side profile
	// summary (onboarding signals + behaviour counts) — feeds the staff
	// profile API's transparency view.
	DatalakeProfile = "/v1/datalake/profile"
	// DatalakeCompact (POST) bin-packs every sink table's small Parquet
	// files. Lives on the pipeline for the same single-writer reason as the
	// purge: a compaction commit from another process would race the sink's
	// flush on Delta version allocation (which is exactly what the old
	// standalone cmd/compact CronJob risked). Triggered by the
	// batch-conductor's chain.
	DatalakeCompact = "/v1/datalake/compact"
)

// ============================================================
// Exchange (:8081) - auction pipeline
// ============================================================

const (
	OpenRTBAuction = "/v1/openrtb/auction"
	OpenRTBWin     = "/v1/openrtb/win"
	OpenRTBLoss    = "/v1/openrtb/loss"
	// PrebidAuction is the Prebid Server-compatible bidder endpoint.
	// External Prebid Server instances POST OpenRTB 2.x bid requests here;
	// we apply our floor policy then dispatch through the normal auction.
	// See pkg/prebid + docs/PLAN.md → "Prebid Server Integration".
	PrebidAuction = "/v1/prebid/openrtb2/auction"
	// PrebidSetUID is the cookie-sync endpoint Prebid bidders expose so
	// Prebid Server can map publisher-side user IDs to our internal IDs.
	PrebidSetUID = "/v1/prebid/setuid"
)

// ============================================================
// DSP (:8082) - bid evaluation
// ============================================================

const (
	OpenRTBBid   = "/v1/openrtb/bid"
	DSPCampaigns = "/v1/dsp/campaigns"
	DSPShading   = "/v1/dsp/shading"
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
// Publisher Ad Server (:8088) - direct-sold arbitration in front of SSP
// ============================================================
//
// New entry point for ad requests. Runs the arbitration ladder (sponsorship
// → guaranteed → programmatic fallthrough → house) before any programmatic
// auction happens. See docs/PLAN.md → "Publisher-Side Ad Server" and
// pkg/publisheradserver.
const (
	// PublisherAdServe is the visitor-facing endpoint. Same shape as
	// routes.SSPServe but arbitrates direct-sold first. Pub sim and other
	// publisher clients should target this once it's wired.
	PublisherAdServe = "/v1/pubad/serve"
	// PublisherAdServeVAST returns a VAST 4.2 XML document for the video
	// flow. Players (IMA SDK, video.js, hls.js) fetch from this endpoint
	// and parse the response to discover the ad media file + tracker
	// URLs. Same auction shape under the hood as PublisherAdServe — the
	// only difference is the response is XML, not JSON.
	PublisherAdServeVAST = "/v1/pubad/video/vast"
	// PublisherAdServeVMAP returns a VMAP 1.0 schedule describing one
	// or more ad breaks (pre-roll / mid-roll / post-roll). Each break's
	// AdSource is an AdTagURI pointing at PublisherAdServeVAST so the
	// player fetches a fresh, independent VAST per break — separate
	// auctions, independent winners, the shape long-form publishers
	// expect for instream video.
	PublisherAdServeVMAP = "/v1/pubad/video/vmap"
	// PublisherAdServeNative renders an OpenRTB Native 1.2 winner into an HTML
	// fragment with signed impression/click trackers. Same auction shape as
	// PublisherAdServe under the hood (SSP channel=native), but the native
	// response markup is assembled here rather than fetched as creative HTML.
	PublisherAdServeNative = "/v1/pubad/native"
	// PublisherAdServeAudio returns a VAST 4.2 document carrying an AUDIO
	// MediaFile (audio/mpeg, no width/height) for the audio flow — podcast /
	// streaming-radio players fetch this the way video players fetch VAST.
	// Same auction shape as the video path (SSP channel=audio); the only
	// differences are the audio MediaFile and quartile beacons routed through
	// /v1/t/audio instead of /v1/t/video.
	PublisherAdServeAudio = "/v1/pubad/audio"
	// AdCertKey serves the exchange's ads.cert Ed25519 public key so DSPs can
	// fetch it (and pick up rotations) instead of hardcoding it in config.
	AdCertKey = "/v1/adcert/key"
)

// ============================================================
// SSAI Stitcher (:8093) - server-side ad insertion
// ============================================================

const (
	// SSAIManifest returns an HLS media playlist with ads stitched into the
	// content stream. The player fetches this instead of the origin manifest;
	// it runs a per-break auction (SSP channel=video), replaces the content-
	// during-break segments with ad segments, and fires the impression beacon
	// server-side. Query: content (origin key), placement_id, plus the usual
	// geo/device/consent/identity signals.
	SSAIManifest = "/v1/ssai/manifest.m3u8"
	// SSAIManifestMPD is the DASH counterpart of SSAIManifest: the same stitch
	// pipeline rendered as a multi-period MPEG-DASH MPD (over CMAF segments).
	SSAIManifestMPD = "/v1/ssai/manifest.mpd"
	// SSAISegment is the per-ad-segment beacon+redirect endpoint referenced by
	// the stitched manifest. When the player fetches an ad segment, this fires
	// the segment's quartile beacon server-side (the SSAI beacon model) and
	// 302-redirects to the real media. Query: session, ad, event, redir.
	SSAISegment = "/v1/ssai/seg"
	// SSAIContent serves a sample origin content manifest (with CUE-OUT/CUE-IN
	// ad-break markers) so the stitcher has something to rewrite in the demo.
	SSAIContent = "/v1/ssai/content.m3u8"
)

// ============================================================
// Simulator support (served by the gateway)
// ============================================================

const (
	// SimRealism returns the consent + identity query-param encoding for a
	// (consent regime, identity type) selection, built by pkg/simulator/request.
	// The web publisher-simulator fetches this instead of re-implementing the
	// TCF / GPP / UID2 encoding in JS — one source of truth shared with the CLI.
	SimRealism = "/v1/sim/realism"
	// SimPersonas returns the simulator persona registry as JSON.
	SimPersonas = "/v1/sim/personas"
)

// ============================================================
// Transcoder (:8094) - runtime ad conditioning for SSAI
// ============================================================

const (
	// TranscodeCondition conditions an ad (transcode + segment to a content
	// profile) into HLS and caches it in the object store. The SSAI stitcher
	// calls this per ad break so the winning ad's segments are byte-compatible
	// with the content stream. Internal (ssai → transcoder); no gateway proxy.
	TranscodeCondition = "/v1/transcode/condition"
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
	// ReportingTrace reconstructs a single request's flow (scoped + redacted per
	// caller). ReportingRecentImpressions lists recent impressions to inspect.
	ReportingTrace             = "/v1/reporting/trace"
	ReportingRecentImpressions = "/v1/reporting/recent-impressions"
	BillingSummary             = "/v1/billing/summary"
	BillingLedger              = "/v1/billing/ledger"
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
	// DebugAudienceRefresh forces a synchronous audience preloader run on
	// services that hold the warm-preload variant (DSP, SSP). Used by e2e
	// tests after inserting audience_segment_members so the bid path sees
	// the new row immediately instead of waiting up to 30s for the next
	// natural preload tick.
	DebugAudienceRefresh = "/debug/audience/refresh"
	// DebugAuctionWins returns the count of auction-win records the reporting
	// in-memory analytics store has for a given trace_id. Used by e2e tests
	// to verify adtech.auction.win NATS events reached reporting exactly once.
	DebugAuctionWins = "/debug/auction_wins"
	// DebugBudgetDepletions counts BudgetDepletedEvent records for a campaign.
	DebugBudgetDepletions = "/debug/budget_depletions"
	// DebugCampaignStateChanges returns the recorded state transitions
	// (live → paused / archived / etc.) for a campaign_id, in arrival
	// order. Used by e2e tests to verify adtech.campaign.state_changed
	// events reached reporting.
	DebugCampaignStateChanges = "/debug/campaign_state_changes"
	// DebugTrackerRejections returns the recorded tracker-rejection
	// records, filterable by ?trace_id= and/or ?reason= (one of
	// invalid_signature / fraud / dedup). Used by e2e to verify the
	// adtech.tracker.rejected pathway + by ops dashboards.
	DebugTrackerRejections = "/debug/tracker_rejections"
	// DebugRenderFailures returns ad-server render-failure records for
	// a ?creative_id=. Used by e2e to verify adtech.adserver.render_failed
	// + by ops dashboards to surface broken creatives.
	DebugRenderFailures = "/debug/render_failures"
	// DebugFreqCapBlocks returns suppression records for a
	// ?campaign_id=. Used by e2e to verify adtech.adserver.freq_cap_blocked
	// + by ops dashboards to surface over-cap volume.
	DebugFreqCapBlocks = "/debug/freq_cap_blocks"
	// DebugServeNoFills counts ServeNoFill records for a trace_id.
	DebugServeNoFills = "/debug/serve_nofills"
	// DebugMediaEvents counts MediaEvent records for a trace, optionally
	// filtered by ?channel=video|audio and ?event_type=start|complete|...
	DebugMediaEvents = "/debug/media_events"
	// DebugBillingReset wipes the in-memory billing ledger so e2e billing
	// tests can run in isolation without inheriting state from prior tests
	// in the same reporting pod lifetime. No-op on TigerBeetle backend.
	DebugBillingReset = "/debug/billing/reset"
	// DebugSpendSnapshot returns the billing engine's current per-campaign
	// committed spend (settled + open reserves, in cents); POST additionally
	// forces an immediate publish of the spend snapshot so e2e tests can drive
	// DSP pacing reconciliation deterministically without waiting for the
	// periodic ticker. Also handy for ops ("what does billing think campaign X
	// has committed today?").
	DebugSpendSnapshot = "/debug/spend/snapshot"
	// DebugBillingRates dumps the in-memory ContractStore (per-publisher
	// revenue-share contracts loaded from publishers.revshare_config).
	// Read by the pub sim's Billing Rates panel so operators can see why
	// a settle produced a particular publisher payout.
	DebugBillingRates = "/debug/billing/rates"
	// Exchange debug surface — internal-state dumps + router preview/reset.
	// All gated by debug.endpoints_enabled; do NOT expose externally.
	DebugExchangeDeals   = "/debug/exchange/deals"
	DebugExchangeRouting = "/debug/exchange/routing"
	// Publisher-adserver debug surface — direct-sold line item cache dump.
	DebugPubAdLineItems = "/debug/pubad/line-items"
	// DebugRollupRun triggers a synchronous rollup run for ?level=minute|
	// hourly|daily|monthly (default minute). Lets ops + e2e force a rollup
	// without waiting for the scheduler tick. Returns the per-config results.
	DebugRollupRun = "/debug/rollup/run"
)

// ============================================================
// Service base URLs (default local dev)
// ============================================================

// DefaultHost is the base hostname for local dev. Override via config for staging/prod.
const DefaultHost = "localhost"

const (
	DefaultGatewayURL           = "http://" + DefaultHost + ":" + PortGateway
	DefaultExchangeURL          = "http://" + DefaultHost + ":" + PortExchange
	DefaultDSPURL               = "http://" + DefaultHost + ":" + PortDSP
	DefaultTrackerURL           = "http://" + DefaultHost + ":" + PortTracker
	DefaultSSPURL               = "http://" + DefaultHost + ":" + PortSSP
	DefaultAdServerURL          = "http://" + DefaultHost + ":" + PortAdServer
	DefaultReportingURL         = "http://" + DefaultHost + ":" + PortReporting
	DefaultPipelineURL          = "http://" + DefaultHost + ":" + PortPipeline
	DefaultPublisherAdServerURL = "http://" + DefaultHost + ":" + PortPublisherAdServer
	DefaultSSAIURL              = "http://" + DefaultHost + ":" + PortSSAI
	DefaultTranscoderURL        = "http://" + DefaultHost + ":" + PortTranscoder
	DefaultNATSURL              = "nats://" + DefaultHost + ":" + PortNATSClient
	DefaultDSPComp1URL          = "http://" + DefaultHost + ":" + PortDSPComp1
	DefaultDSPComp2URL          = "http://" + DefaultHost + ":" + PortDSPComp2
	DefaultJaegerURL            = "http://" + DefaultHost + ":" + PortJaeger
	// Infrastructure addresses. These point at the local Tilt-managed
	// services. Staging/prod overlays override via env vars or the config
	// manager — the constants exist so dev tools and the test harness don't
	// hardcode the same strings independently.
	DefaultRedisAddr     = DefaultHost + ":" + PortRedis
	DefaultMinioEndpoint = DefaultHost + ":" + PortMinioAPI
	DefaultPostgresURL   = "postgres://adtech:adtech-local-dev@" + DefaultHost + ":" + PortPostgres + "/adtech?sslmode=disable"
	// DefaultClickHouseHTTPURL is ClickHouse's HTTP interface (Tilt forwards
	// 8123; the native protocol is on 9010 locally to avoid Minio's 9000).
	// Same local-dev credentials convention as DefaultPostgresURL.
	DefaultClickHouseHTTPURL = "http://adtech:adtech-local-dev@" + DefaultHost + ":" + PortClickHouseHTTP
	// DefaultClickHouseNativeAddr is ClickHouse's native protocol (Tilt
	// forwards host 9010 → cluster 9000; 9000 on the host is taken).
	DefaultClickHouseNativeAddr = DefaultHost + ":" + PortClickHouseNative
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
	PortGateway           = "8080"
	PortExchange          = "8081"
	PortDSP               = "8082"
	PortDSPComp1          = "8089"
	PortDSPComp2          = "8090"
	PortTracker           = "8083"
	PortSSP               = "8084"
	PortAdServer          = "8085"
	PortReporting         = "8086"
	PortPipeline          = "8087"
	PortPublisherAdServer = "8088"
	PortWebhooks          = "8091"
	PortIdentityConsumer  = "8092"
	PortSSAI              = "8093"
	PortTranscoder        = "8094"
	PortReportRunner      = "8095"
	PortClickHouseHTTP    = "8123"
	PortClickHouseNative  = "9010"
	PortGrafana           = "3000"
	PortPrometheus        = "9090"
	PortJaeger            = "16686"
	PortNATSClient        = "4222"
	PortNATSMonitor       = "8222"
	PortPostgres          = "5432"
	PortRedis             = "6379"
	PortMinioAPI          = "9000"
	PortMinioUI           = "9001"
)
