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
	// APIAttribution backs the portal attribution view — proxied to reporting's
	// ReportingAttribution, tenant-scoped per the injected account headers.
	APIAttribution = apiPrefix + "/api/attribution"

	// Gateway-local CRUD: secrets management (operator-only). Reads the
	// secrets table directly and publishes adtech.cache.invalidate.secrets
	// on every mutation so service warm caches re-sync sub-second. Gated
	// by middleware.AuthAPIKey, not the JWT proxy chain — the Secrets
	// tab in /dev/console drives this.
	APISecrets = apiPrefix + "/api/secrets"
	// APIAudiences is the CRM/audience upload endpoint (create segment +
	// bulk-add members). POST only.
	APIAudiences = apiPrefix + "/api/audiences"
	// APITaxonomy lists the IAB Audience Taxonomy reference nodes (migration
	// 062) for the portal's segment-labelling picker. GET only; global
	// reference data, same list for every account.
	APITaxonomy = apiPrefix + "/api/taxonomy"
	// APIAudienceTaxonomy sets/clears a segment's IAB Audience Taxonomy label:
	// PUT {segment_id, taxonomy_id|null}. Labelled PUBLIC segments are what
	// the SSP expresses to external bidders as user.data (ext.segtax).
	APIAudienceTaxonomy = apiPrefix + "/api/audiences/taxonomy"
	// APIAudienceFee sets/clears a segment's data fee (migration 063): PUT
	// {segment_id, data_fee_micros|null} — CPM in micro-dollars the owner
	// earns per delivered impression when an EXTERNAL buyer wins a request
	// carrying this public, taxonomy-labelled segment.
	APIAudienceFee = apiPrefix + "/api/audiences/fee"
	// APIAudienceEarnings lists the account's accrued data-fee earnings per
	// segment (GET) — impressions, gross fee, platform margin, owner net.
	APIAudienceEarnings = apiPrefix + "/api/audiences/earnings"
	// APIAudienceIngest is the audience-ingest job status endpoint (ADR 0007):
	// GET /v1/api/audiences/ingest/{id} returns the tenant-scoped job's status,
	// counts, match rate, and error. Registered as a subtree so {id} is a path
	// segment.
	APIAudienceIngest = apiPrefix + "/api/audiences/ingest/"
	// APIAudiencePGPKey serves the platform's PGP PUBLIC key (ADR 0008) so
	// providers can encrypt audience files to it before upload: GET returns
	// {public_key, fingerprint}. The key is public, but the endpoint sits behind
	// the app (JWT-gated tenant user). 404/503 when no key is configured.
	APIAudiencePGPKey = apiPrefix + "/api/audiences/pgp-key"
	// APIIntegrationAdsTxt tells a publisher how to authorise this platform in
	// their ads.txt: GET returns {seller_domain, seller_id, relationship, line,
	// enforcement, sellers_json_url, configured}. Read from the same shared
	// exchange.adstxt_seller_domain/id the exchange enforces on, so the surfaced
	// line always matches what strict mode checks. JWT-gated.
	APIIntegrationAdsTxt = apiPrefix + "/api/integration/adstxt"
	// APIAudienceMappings is tenant-scoped custom field mappings ("connectors",
	// ADR 0008 Feature 3): GET lists the account's saved mappings, POST
	// creates/updates one ({name, mappings, id_type}), DELETE .../{id} removes one.
	// Registered as both the base path and a subtree (.../{id}) — see main.go.
	APIAudienceMappings = apiPrefix + "/api/audiences/mappings"
	// APIAudienceMappingSample builds a mapping from a small sample upload (ADR
	// 0008): POST a sample file, get back its detected (lowercased) columns + a
	// suggested id_value mapping. More specific than APIAudienceMappings, so it
	// registers before the mappings subtree.
	APIAudienceMappingSample = apiPrefix + "/api/audiences/mappings/sample"
	// APIAudienceProviders is the tenant-scoped data-provider registry (ADR 0009,
	// the DMP foundation): GET lists the account's providers, POST creates/updates
	// one, DELETE .../{id} removes one. A provider carries a data-party
	// classification, licence, default id_type, encryption contract, and
	// notification defaults that an upload snapshots when it selects the provider.
	// Registered as both the base path and a subtree (.../{id}) — see main.go.
	APIAudienceProviders = apiPrefix + "/api/audiences/providers"
	// APIConversions is advertiser conversion-event setup: define named
	// conversion types (GET list, POST create, DELETE ?id=) and get an
	// embeddable tracker pixel/snippet per config. JWT-gated on campaigns:read
	// (read) / campaigns:write (mutate); tenant-scoped to the caller's account.
	APIConversions = apiPrefix + "/api/conversions"
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

	// APINotifications is the per-account in-app notification feed for the
	// portals. GET lists recent notifications + the unread count; POST
	// /v1/api/notifications/read {id} | {"all":true} marks read. JWT-gated;
	// tenant-scoped — every read/mutation is bound to the session account.
	APINotifications = apiPrefix + "/api/notifications"

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

	// APIPayoutMethod is the publisher's payout destination + minimum-payout
	// threshold config (GET reads earnings:view, PUT upserts on earnings:manage).
	// Tenant-scoped; the raw destination fields are stored but never returned —
	// reads expose only a masked tail.
	APIPayoutMethod = apiPrefix + "/api/payout-method"

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

	// APIInvoices is the advertiser invoice history (GET list only). JWT-gated
	// on billing:view; tenant-scoped; read-only — invoices are written by the
	// invoice-runner CronJob from billed committed spend. APIInvoiceDetail
	// serves GET /v1/api/invoices/{id}: the invoice header + its per-campaign
	// line items; a cross-tenant id 404s.
	APIInvoices      = apiPrefix + "/api/invoices"
	APIInvoiceDetail = apiPrefix + "/api/invoices/"

	// APIPublishers proxies to the SSP publishers list (GET only,
	// placements:read). Tenant-scoped SSP-side via the forwarded identity —
	// a publisher session gets only its own publishers; platform users all.
	APIPublishers = apiPrefix + "/api/publishers"

	// APIAccounts lists advertiser + publisher accounts for the staff
	// impersonation picker (GET only, support:read). Platform-wide, read-only.
	APIAccounts = apiPrefix + "/api/accounts"

	// APIBillingTerms is the staff advertiser-billing-terms editor
	// (GET ?account_id= on support:read, PUT on support:update). Sets an
	// advertiser account's payment_terms ('prepay'|'invoiced') + credit_limit;
	// invoiced accounts may bid on credit up to the limit. UPSERTs
	// advertiser_balances and publishes the balance cache-invalidate so the DSP
	// gate re-reads the new terms within NATS RTT. Staff-only — advertisers
	// see their mode read-only (via APIBillingTopup) but cannot change it.
	APIBillingTerms = apiPrefix + "/api/billing/terms"

	// APIAuditLog is the staff audit-log viewer (GET only, audit:read).
	// Platform-wide by design (operator tool) with exact-match filters:
	// ?account_id=&action=&resource_type=&resource_id=&limit=.
	APIAuditLog = apiPrefix + "/api/audit"

	// APIProfiles is the staff profile-transparency lookup (GET /{id},
	// support:read): identity cluster + direct graph links + memberships
	// with provenance + lake signal summary — "what do we know about this
	// user", trace-explorer style. Platform-wide by design (operator tool).
	APIProfiles = apiPrefix + "/api/profiles"

	// APIDemoOnboarding is the staff-only guided "Onboarding & Expansion" demo
	// (GET, support:read): returns the last-run snapshot (or a "not run yet"
	// shape) of the 5-step upload→cluster→expand→serve story over an isolated
	// synthetic demo account.
	APIDemoOnboarding = apiPrefix + "/api/demo/onboarding"
	// APIDemoOnboardingRun runs the demo (POST, support:update): RESETs the
	// isolated demo account, re-seeds the synthetic identity edges, re-creates
	// the empty "Demo Newsletter" segment, runs the real profile-builder, and
	// returns the assembled 5-step timeline.
	APIDemoOnboardingRun = apiPrefix + "/api/demo/onboarding/run"

	// APIDemoTrace is the staff-only guided "Auction Trace" demo
	// (GET, support:read): returns the last-run snapshot (or a "not run yet"
	// shape) of the 5-step persona→request→auction→serve→pipeline story built
	// from a single real ad request's trace.
	APIDemoTrace = apiPrefix + "/api/demo/trace"
	// APIDemoTraceRun runs the demo (POST, support:update): fires ONE fixed-
	// persona ad request through the real serve path (SSP /v1/ssp/serve),
	// captures the X-Trace-Id, polls the trace reader until the impression
	// event lands (async via NATS→reporting), and returns the assembled 5-step
	// timeline of that request's journey.
	APIDemoTraceRun = apiPrefix + "/api/demo/trace/run"

	// APIDemoBilling is the staff-only guided "Billing / Money Flow" demo
	// (GET, support:read): returns the last-run snapshot (or a "not run yet"
	// shape) of the 5-step win→impression→ledger→balance→invoice story built
	// from a single real impression's drawdown.
	APIDemoBilling = apiPrefix + "/api/demo/billing"
	// APIDemoBillingRun runs the demo (POST, support:update): fires ONE fixed-
	// persona request through the real serve path so a real advertiser wins,
	// snapshots the winner's prepay balance + committed spend, fires the real
	// impression beacon (the billable event), polls Postgres until the drawdown
	// lands, and returns the assembled 5-step before→after money-flow timeline.
	APIDemoBillingRun = apiPrefix + "/api/demo/billing/run"

	// APIDemoRollups is the staff-only guided "Rollups" demo
	// (GET, support:read): returns the last-run snapshot (or a "not run yet"
	// shape) of the 5-step firehose→landed→collapse→coarser→ladder story that
	// shows how raw impression rows collapse into far fewer aggregated rows
	// (same totals) as you climb the rollup ladder.
	APIDemoRollups = apiPrefix + "/api/demo/rollups"
	// APIDemoRollupsRun runs the demo (POST, support:update): fires N real
	// fixed-persona impressions through the serve path, waits for them to land,
	// then asks the reporting query API for the raw impression row count and the
	// same count grouped by the rollup dimensions — one row per dimension-tuple,
	// which IS what an hourly rollup row is — proving the collapse (fewer rows,
	// identical totals). Returns the assembled 5-step timeline.
	APIDemoRollupsRun = apiPrefix + "/api/demo/rollups/run"

	// APIDemoRetargeting is the staff-only guided "Retargeting" demo
	// (GET, support:read): returns the last-run snapshot (or a "not run yet"
	// shape) of the 5-step visit→behaviour-row→rule→segment→expand story that
	// shows how one synthetic user visiting an advertiser's page (the
	// retargeting pixel) becomes a member of a retargeting audience. The
	// behavioural/lake sibling of the onboarding demo.
	APIDemoRetargeting = apiPrefix + "/api/demo/retargeting"
	// APIDemoRetargetingRun runs the demo (POST, support:update): RESETs the
	// isolated demo account, re-seeds the synthetic identity edges + a
	// site_visit retargeting-rule segment, fires the real /v1/t/rt pixel AND
	// writes the site_visit behaviour_signals row to the lake, runs the REAL
	// lake-backed profile-builder so its behavioural rule matches the row,
	// enrols the person and cluster-expands the membership, and returns the
	// assembled 5-step before→after timeline.
	APIDemoRetargetingRun = apiPrefix + "/api/demo/retargeting/run"

	// APIBatchRuns is the staff batch-conductor monitor (GET, support:read):
	// recent chain runs with per-step status — what ran, in what order,
	// what failed, what got skipped.
	APIBatchRuns = apiPrefix + "/api/batch/runs"

	// APIBatchLake is the staff lake-state view (GET, support:read):
	// proxies the pipeline's datalake snapshot (per-table rows / active
	// files) for the Batch runs troubleshooting page.
	APIBatchLake = apiPrefix + "/api/batch/lake"

	// APIOnboardingRuns is the staff drop-zone monitor (GET, support:read):
	// recent audience_ingest_jobs (ADR 0007; incl. queued/running) + per-provider
	// rollups (files, rejects, match rates). Platform-wide operational telemetry.
	APIOnboardingRuns = apiPrefix + "/api/onboarding/runs"

	// Staff ops console (/v1/api/ops/*) — monitor AND act on the k8s stack
	// from the staff portal. Reads gated ops:read; mutations ops:deploy, every
	// one audit-logged. Served by the gateway's in-cluster kubeops client
	// (503 off-cluster).
	APIOpsPods           = apiPrefix + "/api/ops/pods"             // GET pod matrix
	APIOpsReadyzGrid     = apiPrefix + "/api/ops/readyz-grid"      // GET per-service /readyz fan-out
	APIOpsNATS           = apiPrefix + "/api/ops/nats"             // GET JetStream stream/consumer lag summary
	APIOpsCronJobs       = apiPrefix + "/api/ops/cronjobs"         // GET cronjob list
	APIOpsCronJobTrigger = apiPrefix + "/api/ops/cronjobs/trigger" // POST {name} — run a cronjob now
	APIOpsJobs           = apiPrefix + "/api/ops/jobs"             // GET jobs (?label_selector=)
	APIOpsPVCs           = apiPrefix + "/api/ops/pvcs"             // GET persistent volume claims
	APIOpsLogs           = apiPrefix + "/api/ops/logs"             // GET ?pod=&container=&tail= log tail
	APIOpsRestart        = apiPrefix + "/api/ops/restart"          // POST {deployment} — rollout restart

	// APIRevshare is the staff revenue-share editor (GET list on support:read,
	// PATCH ?id= on support:update). Platform-wide commercial term; updates
	// publishers.revshare_config and invalidates the billing-rates cache so
	// reporting's ContractLoader re-reads the split.
	APIRevshare = apiPrefix + "/api/revshare"
	// APIMyRevshare is the publisher-scoped read of the caller's OWN revenue-
	// share terms (GET, earnings:view) — so the publisher portal can show net
	// (post-fee) earnings and the fee split without the staff-only editor.
	APIMyRevshare = apiPrefix + "/api/my-revshare"

	// APIHouseAds is the staff house-ad editor (GET list on support:read;
	// POST create / PUT ?id= update / DELETE ?id= on support:update). House
	// ads are the platform's OWN fallback creatives served on a no-bid when
	// publisher_adserver.stub_on_nobid is on. Platform-global (no tenant
	// scope); mutations publish the house-ads cache invalidate so the
	// publisher ad server reloads sub-second. APIHouseAds+"/" serves by-id
	// PUT/DELETE via a trailing-slash path.
	APIHouseAds = apiPrefix + "/api/house-ads"

	// APIHouseAdsFill is the master on/off for serving house ads on a no-bid
	// (GET current state on support:read; PUT {enabled} on support:update). It
	// sets the GLOBAL publisher_adserver.stub_on_nobid config row AND clears any
	// per-pod override rows for that key, so the switch actually takes effect
	// platform-wide (a per-pod row would otherwise shadow the global value).
	// Registered as an exact path so it wins over the APIHouseAds+"/" by-id
	// handler.
	APIHouseAdsFill = apiPrefix + "/api/house-ads/fill"

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

// The pipeline's /v1/datalake/{purge,residual,profile,compact,reset,vacuum}
// endpoints were retired with the Delta dual-write sink (ADR 0006 phase 5). The
// lake is now a derived hourly ClickHouse→Parquet export owned by reporting; GDPR
// deletion of the user-keyed tables runs in ClickHouse (privacydelete.SignalsPurger).

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
	// TrackerRetarget is the retargeting pixel advertisers embed on THEIR OWN
	// sites (?aid=<account>&tag=<label>&uid=<hashed id> + consent params).
	// Consent-gated at capture; publishes a site_visit behaviour row the
	// profile-builder turns into retargeting-segment memberships.
	TrackerRetarget = "/v1/t/rt"
	TrackerView     = "/v1/t/view"
	TrackerVideo    = "/v1/t/video"
	TrackerAudio    = "/v1/t/audio"
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
	// ReportingRollupRun triggers a synchronous rollup for
	// ?level=minute|hourly|daily|monthly (&lookback=N completed windows,
	// default 1). First-class internal route — the batch-conductor's chain
	// step drives it, so it must not depend on debug.endpoints_enabled the
	// way its /debug alias does.
	ReportingRollupRun = "/v1/reporting/rollup/run"
	// ReportingTrace reconstructs a single request's flow (scoped + redacted per
	// caller). ReportingRecentImpressions lists recent impressions to inspect.
	ReportingTrace             = "/v1/reporting/trace"
	ReportingRecentImpressions = "/v1/reporting/recent-impressions"
	// ReportingAttribution returns a conversion's multi-touch chain with
	// per-touchpoint credit apportioned under a requested model.
	ReportingAttribution = "/v1/reporting/attribution"
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
	// DebugDSPBudget (GET ?campaign_id=…) returns the DSP's per-campaign daily
	// spend counter (dsp:budget:{day}:{cid}:spent) in micro-dollars — the value
	// the pacing gate reads and the spend-snapshot reconcile overwrites. Lets
	// e2e observe pacing/reconcile at the DSP. Gated by debug.endpoints_enabled.
	DebugDSPBudget = "/debug/budget"
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
	// ReportingExportRun (POST ?hour=RFC3339) snapshots the ClickHouse event +
	// signal tables for one hour to Parquet on the lake bucket via s3()
	// (ADR 0006 phase 4). Idempotent per hour; defaults to the previous full
	// hour. The batch-conductor calls it hourly; ops/e2e can force a run.
	ReportingExportRun = "/debug/export/run"
	// ReportingExportSnapshot (GET) reports total exported row count per table
	// across the Parquet export — the "did every event reach the archive?"
	// reconciliation that replaced the retired Delta /debug/datalake/snapshot
	// (ADR 0006 phase 5). Same {table:{total_rows:n}} shape as the old endpoint.
	ReportingExportSnapshot = "/debug/export/snapshot"
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
	PortNotifications     = "8096"
	// 81xx = internal gRPC twin of the service's 80xx HTTP port. Only edges
	// where this platform owns both ends listen here (see pkg/grpcx);
	// external boundaries stay OpenRTB/HTTP.
	PortExchangeGRPC = "8181"
	PortDSPGRPC      = "8182"
	PortAdServerGRPC = "8185"

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
