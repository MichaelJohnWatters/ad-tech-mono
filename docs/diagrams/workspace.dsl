workspace "Ad Tech Mono" "C4 model of the full-stack programmatic advertising platform. Context + Container are complete; Component views are filled in service-by-service (see docs/diagrams/C4-EXPANSION-PROMPT.md)." {

  model {
    # ---- People / actors ----
    advertiser = person "Advertiser" "Runs campaigns, uploads audiences, views reports."
    publisher  = person "Publisher" "Manages inventory/placements, views yield + payouts."
    staff      = person "Staff / Ops" "Platform ops, moderation, incidents, impersonation."
    enduser    = person "End user (browser / app)" "Sees ads; fires impression/click/view beacons."

    # ---- External systems ----
    competitorDSP = softwareSystem "Third-party / competitor DSPs" "External OpenRTB bidders the exchange fans out to." "External"
    prebid        = softwareSystem "Prebid Server" "Header-bidding demand the publisher-adserver fans out to." "External"
    oidc          = softwareSystem "OIDC Identity Provider" "Per-account SSO (auth-code + PKCE)." "External"
    smtp          = softwareSystem "Email (Mailpit / SES)" "Verification, reports, notifications." "External"

    # ---- The platform ----
    adtech = softwareSystem "Ad Tech Platform" "Programmatic advertising platform — SSP, exchange, DSP, ad servers, data + money pipelines." {

      gateway = container "Gateway" "Auth (JWT/SSO/API-key), RBAC, HTMX portals, REST API, proxy to internal gRPC." "Go" {
        gw_authJwt       = component "JWT / API-Key Auth" "Validates Bearer JWT / session cookie / API key; strips then re-injects identity headers. (pkg/middleware/auth.go, api_key.go)" "Go"
        gw_ssoHandler    = component "OIDC SSO Handler" "Per-account auth-code + PKCE flow, id_token verify, JIT least-privilege provisioning. (cmd/gateway/sso.go, pkg/ssoauth)" "Go"
        gw_rbacAuthz     = component "RBAC Authorizer" "Enforces resource:action permissions + account-type; method-aware gates; agency act-as. (pkg/auth, pkg/middleware)" "Go"
        gw_sessionMgr    = component "Session Manager" "httpOnly JWT cookie + Redis iat-checkpoint revocation (logout-everywhere). (cmd/gateway/auth_login.go, pkg/middleware/revocation.go)" "Go"
        gw_portalRender  = component "HTMX Portal" "Renders the advertiser/publisher/staff/partner portals with permission-filtered nav. (cmd/gateway/portal.go)" "Go"
        gw_restHandlers  = component "REST API Handlers" "Tenant-scoped CRUD (campaigns/creatives/placements/audiences/reports/billing/webhooks…). (cmd/gateway/*.go)" "Go"
        gw_reverseProxy  = component "Reverse Proxy" "Strips prefix, injects X-Account/User from claims, forwards to dsp/ssp/adserver/reporting. (pkg/middleware/proxy.go)" "Go"
      }

      # --- Serving hot path ---
      ssp         = container "SSP" "Supply-side: publisher inventory, builds bid requests, resolves + stamps audience segments (consent-gated)." "Go" {
        ssp_requestHandler  = component "Request Handler" "Serves /v1/ssp/request: builds the OpenRTB bid request, orchestrates resolution → auction → render. (cmd/ssp/main.go)" "Go"
        ssp_segmentResolver = component "Segment Resolver" "Resolves the user's PUBLIC audience segments + IAB segtax labels and stamps user.ext.segments/data (the sell-side enrichment DSPs bid against). (pkg/audience)" "Go"
        ssp_consentGate     = component "Consent Gate" "privacy.Evaluate — GDPR/TCF/US-Privacy/GPP; gates segment stamping, identity observe, personalisation. (pkg/privacy)" "Go"
        ssp_floorEngine     = component "Floor Engine" "Floor price by static/time/device/geo rules." "Go"
        ssp_quality         = component "Quality Controls" "Publisher blocklists / allowlists / category filters." "Go"
        ssp_placementCache  = component "Placement Cache" "L1 warm cache of publishers, placements, floors, deals. (pkg/cache)" "Go"
        ssp_exchangeClient  = component "Exchange Client" "gRPC twin client — RunAuction (grpc:// else OpenRTB HTTP). (pkg/grpcx)" "Go"
        ssp_adserverClient  = component "Ad Server Client" "gRPC twin client — render the winning creative." "Go"
      }
      exchange    = container "Exchange" "Runs the auction, DSP fan-out, deal priority (PG>Preferred>PMP>Open), win/loss notices (price + clear_price)." "Go" {
        exch_auctionHandler = component "Auction Handler" "RunAuction (gRPC twin + /v1/openrtb/auction HTTP): orchestrates deals → fan-out → auction → notices. (cmd/exchange/main.go)" "Go"
        exch_partnerAuth    = component "Partner Inbound Auth" "Gates the EXTERNAL OpenRTB surface on per-partner API keys (warn|strict). (middleware)" "Go"
        exch_adstxt         = component "ads.txt Verifier" "Validates seller authorisation before accepting a request (off|warn|strict). (pkg/fraud/adstxt)" "Go"
        exch_dealMatcher    = component "Deal Matcher" "Match + priority PG>Preferred>PMP>Open; PG preempts, Preferred/PMP set the floor. (pkg/deals)" "Go"
        exch_router         = component "SmartRouter" "Fan-out routing — skip slow/deadbeat DSP legs, ε-probe, warm-start from dsp_calls. (pkg/auction/router.go)" "Go"
        exch_auctionEngine  = component "Auction Engine" "First-price (default) + clearing/second-price signal. (pkg/auction)" "Go"
        exch_winLossNotify  = component "Win/Loss Notifier" "sendWinLossNotifications — win/loss nurls carrying price + clear_price (minToWin). (cmd/exchange/main.go)" "Go"
        exch_warmCache      = component "Warm Cache" "L1 DSP endpoints, floors, deals, ads.txt (Redis-backed)." "Go"
      }
      dsp         = container "DSP" "Demand-side: campaign eligibility, targeting, bidding, budget/pacing, bid shading." "Go" {
        bidHandler     = component "Bid Handler" "Per-request bid loop: iterate campaigns → eligibility → pace → shade → bid. (cmd/dsp/main.go)" "Go"
        targeting      = component "Targeting Engine" "Boolean include/exclude expressions + stacked bid modifiers. (pkg/targeting)" "Go"
        audienceLookup = component "Audience Lookup" "Segment membership via Redis sets + identity-graph expansion + household. (cmd/dsp/identity.go, pkg/audience)" "Go"
        shading        = component "Bid Shading" "Win-rate curve per placement; shade toward the (second-price) clearing. (pkg/bidshading)" "Go"
        warmCache      = component "Warm Cache / Refresher" "In-process L1 campaigns + budgets + balances, bulk-refreshed in background (no hot-path I/O). (cmd/dsp/refresh.go)" "Go"
        budgetGate     = component "Budget + Balance Gate" "Spend meters + prepay gate; reconciles to billing snapshots. (cmd/dsp)" "Go"
        winLoss        = component "Win/Loss Handler" "nurl handlers: budget record, publish AuctionLoss/AuctionShade, feed shading curve." "Go"
      }
      adserver    = container "Ad Server" "Creative decisioning + serving, frequency capping, HMAC-signed tracking macros." "Go" {
        ad_serveHandler = component "Serve Handler" "HTTP /v1/ad/serve + gRPC InternalAdServeService.Serve: dispatches the decisioning → render pipeline. (cmd/adserver/main.go)" "Go"
        ad_freqCap      = component "Frequency Cap Manager" "Redis-backed per-user + per-household campaign caps, atomic peek/record Lua (PEEK vs RECORD split). (cmd/adserver/freqcap.go)" "Go"
        ad_resolver     = component "Creative Resolver" "Warm metadata cache + L1 body cache; loads approved creatives from Postgres, fetches bodies from S3 on miss. (cmd/adserver/creatives.go)" "Go"
        ad_macroSig     = component "Macro Substitution + Signer" "Substitutes ${…} macros and builds HMAC-signed impression/click/viewability tracker URLs. (pkg/adserving/macros.go, signing.go)" "Go"
        ad_dpa          = component "Dynamic Product Assembler" "Render-time assembly for format=dynamic_product: reads recent SKUs + catalog, runs the Go template, static {{else}} fallback. (cmd/adserver/dynamic_products.go)" "Go"
        ad_bandit       = component "Creative Bandit" "In-memory Thompson-sampling creative rotation, warm-started from reporting's per-creative CTR histogram. (pkg/optimise/bandit.go)" "Go"
        ad_warmCache    = component "Warm Cache Supervisor" "Creative-metadata + freq-cap-rule warm caches: NATS-invalidate + poll + self-healing Postgres loader. (pkg/cache/warm)" "Go"
        ad_eventsPub    = component "Event Publisher" "Async NATS publish of render_failed + freq_cap_blocked (disk-spool fallback). (cmd/adserver/main.go, pkg/events)" "Go"
      }
      pubAdserver = container "Publisher Ad Server" "Direct-sold vs programmatic arbitration per slot (GAM-like) + Prebid fan-out + guaranteed pacing." "Go" {
        pub_serveOrchestrator = component "Serve Orchestrator" "HTTP serve handler: runs arbitration → direct serve or programmatic fan-out, renders the winner per format. (cmd/publisher-adserver/main.go)" "Go"
        pub_arbitration       = component "Arbitration Engine" "Picks direct-sold vs programmatic per slot on the priority ladder sponsorship>guaranteed>house. (pkg/publisheradserver/arbitration)" "Go"
        pub_directMatcher     = component "Direct-Sold Line-Item Matcher" "Warm-cached publisher line items filtered by status / flight window / placement. (pkg/publisheradserver, cmd/publisher-adserver)" "Go"
        pub_pacing            = component "Delivery Pacing Manager" "Redis per-line-item impression actuals; flags guaranteed items behind pace (PacingDecider). (pkg/publisheradserver/pacing)" "Go"
        pub_prebidFanout      = component "Prebid Fan-Out Client" "Parallel OpenRTB requests to external Prebid Servers; selects the highest non-nobid bid. (pkg/publisheradserver/prebidclient)" "Go"
        pub_sspClient         = component "SSP Demand Client" "Calls our SSP /v1/ssp/serve for the programmatic auction (exchange → DSPs), channel-aware. (cmd/publisher-adserver)" "Go"
        pub_formatHandlers    = component "Format Handlers" "Renders the winner as VAST/VMAP (video), native JSON, audio VAST, or display HTML with signed tracker URLs. (cmd/publisher-adserver/vast.go, vmap.go, native.go, audio.go)" "Go"
        pub_eventPub          = component "Event Publisher" "Publishes DirectWin / PrebidOutboundWin / ServeNoFill for analytics. (cmd/publisher-adserver, pkg/events)" "Go"
      }
      tracker     = container "Tracker" "Impression/click/conversion/view beacons, real-time fraud checks, publishes events." "Go" {
        trk_beacons     = component "Beacon Handlers" "/v1/t/imp|click|conv|view|video|audio: validate → fraud → dedup → publish; click 302-redirects. (cmd/tracker/main.go, mediagate.go)" "Go"
        trk_hmacValidator = component "HMAC Validator" "Validates beacon URL signatures (SHA-256, key-rotation overlap, exp= TTL). (pkg/adserving/signing.go)" "Go"
        trk_fraudScorer = component "Real-Time Fraud Checker" "Inline per-beacon checks: bot UA, datacenter IP, per-IP rate limit, DB blocklists. (pkg/fraud/realtime.go)" "Go"
        trk_blocklistCache = component "Fraud Blocklist Cache" "Warm cache of fraud_blocklists; polls Postgres + NATS-invalidate, pushes IP/UA blocks into the scorer. (cmd/tracker/blocklist.go)" "Go"
        trk_dedup       = component "Dedup Gate" "Redis SetNX on (event_type, trace_id); drops browser retry / double-tap replays. (cmd/tracker/dedup.go)" "Go"
        trk_eventPub    = component "Event Publisher" "Publishes beacon events to NATS with stable Msg-Id; disk-spool fallback on NATS stall. (cmd/tracker/main.go, pkg/events)" "Go"
        trk_retargetPixel = component "Retargeting Pixel + Identity Bridge" "/v1/t/rt site-visit pixel (consent-gated) + publishes first-party identity edges. (cmd/tracker/retargeting.go, main.go)" "Go"
        trk_ara         = component "ARA Handler" "Privacy Sandbox Attribution Reporting API: source/trigger registration + report ingestion. (cmd/tracker/ara.go)" "Go"
      }
      ssai        = container "SSAI Stitcher" "Server-side ad insertion into HLS/DASH manifests, signed segment beacons." "Go" {
        ssai_manifestParser     = component "Manifest Parser" "Parses HLS playlists / DASH MPD, finds CUE-OUT/CUE-IN ad-break spans. (pkg/ssai)" "Go"
        ssai_auctionCaller      = component "Auction Caller" "Runs a per-break auction via the SSP (cap_defer=1 PEEK); mints a distinct trace per pod ad. (cmd/ssai/main.go)" "Go"
        ssai_stitcher           = component "Ad Stitcher" "Splices winning ad segments into breaks (fills→slate→content), handles ad-pod depth. (cmd/ssai/main.go)" "Go"
        ssai_conditioningClient = component "Conditioning Client" "Fetches pre-conditioned ad segments from the transcoder (cache_only); warms async on miss. (cmd/ssai/main.go)" "Go"
        ssai_beaconSigner       = component "Beacon Signer" "Builds HMAC-signed impression + quartile tracker URLs from the auction winner. (pkg/adserving)" "Go"
        ssai_segBeaconHandler   = component "Segment Beacon Handler" "/v1/ssai/seg: fires the pre-signed beacons on segment fetch, 302 to real media. (cmd/ssai/main.go)" "Go"
        ssai_freqCapRecorder    = component "Frequency-Cap Recorder" "RECORDs the confirmed impression against the advertiser cap at stitch time. (cmd/ssai/main.go)" "Go"
      }
      transcoder  = container "Transcoder" "Cache-first ad conditioning (ffmpeg) to content-compatible HLS for SSAI." "Go" {
        tx_httpHandler    = component "Condition Handler" "POST /v1/transcode/condition: branches cache_only vs transcode. (cmd/transcoder/main.go)" "Go"
        tx_cacheLookup    = component "Cache Lookup (S3-first)" "Serving path: checks S3 for pre-conditioned segments, 404 on miss (no transcode). (pkg/transcode/conditioner.go)" "Go"
        tx_ffmpegEngine   = component "FFmpeg Transcode Engine" "Execs ffmpeg to transcode mezzanine → HLS VOD playlist + segments. (pkg/transcode/runner.go)" "Go"
        tx_abrRenditioner = component "ABR Ladder Renditioner" "Config-driven ABR ladder: codec/resolution/bitrate per rung → ffmpeg args. (pkg/transcode/profile.go, ladder.go)" "Go"
        tx_segmentWriter  = component "Segment Writer" "Uploads conditioned playlist + segments to object storage. (pkg/transcode/conditioner.go, pkg/store/objects)" "Go"
      }

      # --- Async / data / money ---
      reporting   = container "Reporting + Billing" "Consumes NATS events → ClickHouse; query API; in-process billing engine (reserve/settle, TigerBeetle); hourly Parquet export." "Go" {
        rep_natsConsumer  = component "NATS Event Consumer" "Consumes impression/click/conversion/view/auction events from JetStream and routes to the writer + billing. (cmd/reporting/main.go)" "Go"
        rep_analyticsWriter = component "Analytics Store Writer" "Persists events to ClickHouse (hot) via analytics.Store; HotColdStore routes old reads to the Parquet lake over s3(). (cmd/reporting/analytics.go, hotcold.go)" "Go"
        rep_billingEngine = component "Billing Engine" "In-process reserve/settle state machine per bid model (CPM/CPC/CPA/vCPM/CPCV) against the ledger. (pkg/billing, cmd/reporting/ledger.go)" "Go"
        rep_balanceSink   = component "Balance Drawdown Sink" "Debits advertiser_balances on settle; publishes balance-depleted + cache-invalidate. (cmd/reporting/balance_sink.go)" "Go"
        rep_pacingPub     = component "Pacing Snapshot Publisher" "Broadcasts per-campaign committed spend (settled+reserved) for DSP pacing reconcile. (cmd/reporting/spend_snapshot.go)" "Go"
        rep_queryEngine   = component "Query / Report API" "HTTP query API: derived metrics (ecpm/ctr/fill), rollup-tier selection, tenant filtering. (cmd/reporting/main.go, pkg/reporting)" "Go"
        rep_exportEngine  = component "Parquet Export Engine" "Hourly ClickHouse→Parquet export to object storage (idempotent per hour). (cmd/reporting/export.go)" "Go"
        rep_traceInspector = component "Trace Inspector API" "Reconstructs the per-request flow timeline, redacted per account type. (cmd/reporting/trace.go)" "Go"
      }
      pipeline    = container "Pipeline" "Publisher-file ingest, audience membership cache-writer (single writer), batch-conductor chain." "Go" {
        pipe_dropzonePoller   = component "Drop-Zone Poller" "Scans the onboarding bucket on interval, enqueues ingest jobs, emails on completion. (cmd/pipeline/onboarding.go)" "Go"
        pipe_ingestWorker     = component "Ingest Worker" "Drains audience_ingest_jobs (SKIP LOCKED + lease) and runs the shared processor. (cmd/pipeline/ingest_worker.go, pkg/ingestjobs)" "Go"
        pipe_ingestProcessor  = component "Ingest Processor" "Shared path: read/decrypt staged file, field-map, AddMembers/UpsertSegment, publish ProfileSignal chunks. (pkg/ingest/processor.go)" "Go"
        pipe_fileProcessor    = component "Decode / Validate Engine" "Auto-detects CSV/TSV/JSON/Parquet/gzip/PGP, validates rows, normalises, quarantines rejects. (pkg/ingest/decode.go, pkg/pipeline)" "Go"
        pipe_membershipWriter = component "Audience Cache Writer (single writer)" "THE single writer: drains the membership changelog to Redis sets (SADD/SREM) + periodic reconcile with TTL + tombstones. (cmd/pipeline/audience_cache_writer.go)" "Go"
      }
      identityConsumer = container "Identity Consumer" "Builds the identity graph from observed IDs (deterministic + probabilistic)." "Go" {
        idc_natsSub      = component "NATS Subscription" "Consumes adtech.identity.observed (queue-grouped), decodes ObservedEvent, acks/naks. (cmd/identity-consumer/main.go)" "Go"
        idc_observer     = component "Observer + Batcher" "Buffers observations, dedupes via seen-set, batches by interval/size, applies linking rules. (pkg/identityobserve/observe.go)" "Go"
        idc_determLinker = component "Deterministic Edge Builder" "Edges for 2+ identifiers co-observed on one request (confidence 1.0). (pkg/identityobserve/observe.go)" "Go"
        idc_probMatcher  = component "Probabilistic Matcher" "IP+UA fingerprint bucketing for same-device heuristic links (skips shared IPs). (pkg/identityobserve/observe.go)" "Go"
        idc_fpStore      = component "Fingerprint Bucket Store" "Redis sets of ids per fingerprint (TTL); in-memory fallback (single-replica). (cmd/identity-consumer/redisfp.go)" "Go"
        idc_edgeWriter   = component "Edge Writer" "Batched idempotent upserts to identity_graph. (pkg/store/postgres/identity.go)" "Go"
      }
      audienceRT  = container "Audience RT" "Real-time retargeting: enroll on site-visit, suppress on purchase." "Go" {
        art_behaviourConsumer  = component "Behaviour Consumer" "Consumes adtech.behaviour.observed (site_visit) and routes to the enroll engine. (cmd/audience-rt/main.go)" "Go"
        art_conversionConsumer = component "Conversion Consumer" "Consumes adtech.events.conversion (purchase) and routes to suppression. (cmd/audience-rt/main.go)" "Go"
        art_enrollEngine       = component "Enroll Engine" "Evaluates single-visit rules (min_count<=1), enrolls visitor+household into matching segments. (pkg/retargeting/retargeting.go)" "Go"
        art_suppressionEngine  = component "Suppression + Cross-Sell Engine" "Suppresses converters (person+household via identity graph), per-SKU burn, cross-sell complements. (pkg/retargeting/retargeting.go)" "Go"
        art_ruleMatcher        = component "Segment Rule Matcher" "Parses segment rule JSON, evaluates event/tag/min_count, resolves TTL window. (pkg/retargeting/retargeting.go)" "Go"
        art_productViews       = component "SKU Retargeting Memory" "Records/recalls viewed/carted SKUs for DPA, burn-list filtered. (pkg/audience/store/postgres/product_views.go)" "Go"
        art_membershipUpsert   = component "Membership Upsert" "Writes/removes segment members with TTL; first-enroll-wins lineage; fires changelog trigger. (cmd/audience-rt/main.go)" "Go"
      }
      reportRunner = container "Report Runner" "Async report jobs (SKIP LOCKED queue) → CSV/Parquet in object storage; export zips." "Go" {
        rr_scheduleEnqueuer = component "Schedule Enqueuer" "Loads due saved-report schedules and enqueues them as jobs. (pkg/reportrunner/runner.go)" "Go"
        rr_jobQueue         = component "Job Queue Manager" "Claim/lease/heartbeat/reclaim on report_jobs (SKIP LOCKED) — multi-replica safe. (pkg/reportjobs/store.go)" "Go"
        rr_renderer         = component "Report Renderer" "Renders query results to CSV / JSON / Parquet (Arrow). (pkg/reportjobs/formats.go, executor.go)" "Go"
        rr_reportingClient  = component "Reporting HTTP Client" "Posts scoped report queries to the reporting service; routes segment exports. (pkg/reportrunner/store.go)" "Go"
        rr_exportZip        = component "Export Zip Builder" "Materialises a per-account data-export zip (campaigns/creatives/invoices/…). (pkg/accountexport/builder.go)" "Go"
        rr_emailNotifier    = component "Email Notifier" "Sends download-link emails on completion (best-effort). (pkg/email/email.go)" "Go"
        rr_artifactStore    = component "Artifact Store" "Uploads artifacts + export zips to the private adtech-reports bucket. (pkg/store/objects)" "Go"
        rr_staleSweeper     = component "Stale-Lease Sweeper" "Reclaims lapsed leases back to queued; GCs expired artifacts. (pkg/reportjobs/sweeper.go)" "Go"
        rr_scopeResolver    = component "Tenant Scope Resolver" "Forces account/publisher scope at enqueue for schedule-driven jobs. (pkg/reportjobs/scope.go)" "Go"
      }
      webhooks    = container "Webhooks" "Delivers business events to registered partner URLs." "Go" {
        wh_natsConsumer      = component "NATS Event Consumer" "Subscribes to account-scoped subjects (budget/balance depleted, campaign state, enrolled, report done); retry-until-stick. (cmd/webhooks/main.go)" "Go"
        wh_eventRouter       = component "Event Router" "Maps NATS subjects → customer event names, extracts account_id, drops poison. (cmd/webhooks/main.go)" "Go"
        wh_dispatcher        = component "Dispatcher" "Looks up subscriptions, wraps the self-describing envelope, fans out per endpoint. (pkg/webhooks/webhooks.go)" "Go"
        wh_subscriptionStore = component "Subscription Store" "Reads active webhook subscriptions for account+event (RLS-scoped). (pkg/webhooks/store_postgres.go)" "Go"
        wh_httpDelivery      = component "HTTP Delivery Client" "POSTs the signed envelope with exponential-backoff retries. (pkg/webhooks/webhooks.go)" "Go"
        wh_payloadSigner     = component "Payload Signer" "HMAC-SHA256 of the body into X-Adtech-Signature. (pkg/webhooks/webhooks.go)" "Go"
        wh_deliveryLog       = component "Delivery Log" "Records each delivery attempt to webhook_deliveries for audit. (pkg/webhooks/store_postgres.go)" "Go"
      }
      notifications = container "Notifications" "In-app portal notifications (the bell)." "Go" {
        ntf_natsConsumer  = component "NATS Event Consumer" "Queue-grouped consumer of account-scoped business events; retry-until-stick. (cmd/notifications/main.go)" "Go"
        ntf_eventMapper   = component "Event → Notification Mapper" "Maps event payloads to Notification rows; drops unmapped/missing-account poison. (pkg/notifications/translate.go)" "Go"
        ntf_postgresWriter = component "Notification Store Writer" "Inserts notifications (RLS GUC + explicit account filter); also the read side for the bell. (pkg/notifications/store_postgres.go)" "Go"
      }
      batchConductor = container "Batch Conductor" "Hourly completion-ordered data chain: rollups → export → profile-builder → privacy purge." "Go (CronJob)" {
        bc_checkpoint   = component "Checkpoint Gate" "CRITICAL first step: probes pipeline + reporting /readyz; aborts the chain if ingestion is down. (pkg/batch/chain.go)" "Go"
        bc_rollupRunner = component "Rollup Runner" "Runs reporting rollup tiers minute→hourly→daily→monthly (finest first). (pkg/batch/chain.go)" "Go"
        bc_exportStep   = component "Parquet Export Step" "Triggers reporting's ClickHouse→Parquet export (idempotent per hour). (pkg/batch/chain.go)" "Go"
        bc_profileBuilder = component "Profile-Builder Step" "In-process clustering → enroll/prune/expand audience memberships (ClickHouse behaviour reads). (pkg/profilebuilder)" "Go"
        bc_privacyDelete = component "Privacy-Delete Step" "In-process GDPR purge of pending opt-outs across Postgres + extras. (pkg/privacydelete)" "Go"
        bc_privacyVerify = component "Privacy-Verify Step" "Residual-PII audit after the purge; red run on incomplete deletions. (pkg/privacydelete)" "Go"
        bc_recorder     = component "Run Recorder" "Writes a batch_runs row per step + announces run_completed on NATS. (cmd/batch-conductor/main.go, pkg/batch)" "Go"
      }

      # --- Datastores ---
      postgres    = container "PostgreSQL" "Transactional store, multi-tenant via RLS (adtech_app NOBYPASSRLS)." "PostgreSQL" "Datastore"
      redis       = container "Redis" "L2: budget/freq-cap counters, audience sets, sessions, rate limits." "Redis" "Datastore"
      clickhouse  = container "ClickHouse" "Analytics event store (impressions, auctions, auction_wins/losses/shades, …)." "ClickHouse" "Datastore"
      nats        = container "NATS JetStream" "Async event bus (JSON payloads)." "NATS" "Datastore"
      storage     = container "Object Storage" "Creatives + the Parquet/Delta lake (Minio local / S3 prod)." "S3" "Datastore"
      tigerbeetle = container "TigerBeetle" "Double-entry billing ledger (spend reserve/settle)." "TigerBeetle" "Datastore"
    }

    # ================= Relationships =================

    # --- Portals / control plane ---
    advertiser -> gateway "Campaigns, audiences, reports, bid-shading view (HTTPS)"
    publisher  -> gateway "Inventory, yield, payouts (HTTPS)"
    staff      -> gateway "Ops, moderation, incidents (HTTPS)"
    gateway -> oidc "SSO auth-code + PKCE"
    gateway -> smtp "Verification / notification email"
    gateway -> dsp "Campaign CRUD (gRPC)"
    gateway -> adserver "Creative CRUD (gRPC)"
    gateway -> ssp "Inventory CRUD (gRPC)"
    gateway -> reporting "Report queries, trace, shading (HTTP)"
    gateway -> postgres "Accounts, sessions, config (RLS)"
    gateway -> redis "Sessions, rate limits"

    # --- Serving hot path ---
    enduser -> ssp "Ad request (web-mirror)"
    enduser -> pubAdserver "Publisher ad request"
    pubAdserver -> ssp "Programmatic demand"
    pubAdserver -> prebid "Header-bidding fan-out (OpenRTB HTTP)"
    ssp -> exchange "Run auction (gRPC twin)"
    exchange -> dsp "Bid request (gRPC twin)"
    exchange -> competitorDSP "Bid request (OpenRTB HTTP)"
    dsp -> exchange "Bid (shaded)"
    exchange -> dsp "Win/loss notice — price + clear_price (HTTP nurl)"
    exchange -> adserver "Winner → render creative"
    adserver -> enduser "Ad markup + signed beacon URLs"
    enduser -> tracker "Impression / click / view beacons"
    ssp -> ssai "Video/CTV stitch request"
    ssai -> transcoder "Condition winning ad"

    # --- Async events ---
    tracker  -> nats "Impression/click/conversion/view + fraud"
    exchange -> nats "AuctionWin / AuctionComplete / DSPCall"
    dsp      -> nats "AuctionLoss / AuctionShade / BudgetDepleted"
    ssp      -> nats "Behaviour observed / data-fee"
    nats -> reporting "Consumes all business events"
    nats -> webhooks "Account-scoped events"
    nats -> notifications "Account-scoped events"
    nats -> identityConsumer "Observed identities"
    nats -> audienceRT "Site-visit / purchase"
    dsp -> nats "consumes cache-invalidate + spend-snapshot"

    # --- Data + money ---
    reporting -> clickhouse "Writes events; serves queries"
    reporting -> tigerbeetle "Ledger reserve/settle"
    reporting -> postgres "Invoices, balances, committed spend"
    reporting -> storage "Hourly Parquet export"
    reporting -> nats "campaign_spend_snapshot (pacing reconcile)"
    pipeline -> redis "Audience cache writer (SADD/SREM)"
    pipeline -> storage "Ingested files / lake"
    pipeline -> postgres "Normalised rows, memberships"
    batchConductor -> reporting "Drives rollups + Parquet export"
    batchConductor -> storage "Profile-builder / privacy purge"
    identityConsumer -> postgres "Identity graph edges"
    audienceRT -> postgres "audience_segment_members (retarget)"
    reportRunner -> clickhouse "Report queries"
    reportRunner -> storage "Report artifacts + export zips"
    reportRunner -> smtp "Download links"

    # --- DSP-internal datastore reads (hot path = in-process; refresher does the I/O) ---
    dsp -> redis "Budget/balance/audience reads"
    dsp -> postgres "Campaign warm-load (cross-tenant loader)"
    ssp -> redis "Audience sets (SMEMBERS)"
    ssp -> postgres "Placements, segments"
    adserver -> redis "Frequency caps"
    adserver -> storage "Creatives"

    # --- DSP component wiring (Component view) ---
    bidHandler -> targeting "Evaluate eligibility"
    bidHandler -> audienceLookup "Resolve the user's segments"
    bidHandler -> shading "Shade the bid toward clearing"
    bidHandler -> budgetGate "Pace + balance gate"
    bidHandler -> warmCache "Read campaigns (in-process)"
    audienceLookup -> redis "SMEMBERS audience sets"
    warmCache -> postgres "Background bulk refresh"
    warmCache -> redis "Budget + balance counters"
    winLoss -> nats "Publish AuctionLoss / AuctionShade"
    winLoss -> shading "Feed win/loss + clear_price into the curve"
    exchange -> winLoss "Win/loss nurl"

    # --- SSP component wiring (Component view) ---
    ssp_requestHandler -> ssp_placementCache "Load placement + floor config"
    ssp_requestHandler -> ssp_consentGate "Evaluate consent"
    ssp_requestHandler -> ssp_segmentResolver "Resolve + stamp public segments"
    ssp_requestHandler -> ssp_floorEngine "Compute floor"
    ssp_requestHandler -> ssp_quality "Apply blocklists / filters"
    ssp_requestHandler -> ssp_exchangeClient "Run auction"
    ssp_requestHandler -> ssp_adserverClient "Render winner"
    ssp_segmentResolver -> ssp_consentGate "Gate on consent"
    ssp_segmentResolver -> redis "SMEMBERS audience sets"
    ssp_placementCache -> postgres "Warm-load placements / floors / deals"
    ssp_exchangeClient -> exchange "RunAuction (gRPC twin)"
    ssp_adserverClient -> adserver "Render creative (gRPC twin)"
    ssp_requestHandler -> nats "Publish behaviour / identity observed (consented)"

    # --- Exchange component wiring (Component view) ---
    exch_auctionHandler -> exch_partnerAuth "Gate external OpenRTB callers"
    exch_auctionHandler -> exch_adstxt "Verify ads.txt pre-fan-out"
    exch_auctionHandler -> exch_dealMatcher "Match deals + priority"
    exch_auctionHandler -> exch_router "Select DSP legs"
    exch_router -> exch_warmCache "Read endpoints + routing stats"
    exch_router -> dsp "Bid request (gRPC twin)"
    exch_router -> competitorDSP "Bid request (OpenRTB HTTP)"
    exch_auctionHandler -> exch_auctionEngine "Run auction → clearing + clear_price"
    exch_auctionHandler -> exch_winLossNotify "Notify winner + losers"
    exch_winLossNotify -> dsp "Win/loss nurl (price + clear_price)"
    exch_auctionHandler -> nats "AuctionWin / AuctionComplete / DSPCall"
    exch_warmCache -> redis "DSP endpoints, floors, deals, ads.txt"

    # --- Ad Server component wiring (Component view) ---
    ad_serveHandler -> ad_freqCap "Resolve + peek/record caps"
    ad_serveHandler -> ad_resolver "Load creative metadata + body"
    ad_serveHandler -> ad_dpa "Assemble dynamic_product creative"
    ad_serveHandler -> ad_macroSig "Substitute macros + sign tracker URLs"
    ad_serveHandler -> ad_bandit "Pick creative arm (Thompson)"
    ad_serveHandler -> ad_eventsPub "render_failed / freq_cap_blocked"
    ad_freqCap -> ad_warmCache "Campaign-scoped cap rule"
    ad_resolver -> ad_warmCache "Creative metadata snapshot"
    ad_freqCap -> redis "Atomic cap counters (Lua)"
    ad_resolver -> storage "Fetch creative body on miss"
    ad_dpa -> postgres "Recent SKUs + product catalog"
    ad_warmCache -> postgres "Poll-load creatives + cap rules"
    ad_warmCache -> nats "Cache-invalidate (creatives / campaigns)"
    ad_eventsPub -> nats "Publish observability events"
    ad_eventsPub -> storage "Event spool (NATS-stall fallback)"
    ssp -> ad_serveHandler "Render winning creative (gRPC twin)"
    ad_bandit -> reporting "Warm-start CTR histogram (HTTP, boot)"

    # --- Publisher Ad Server component wiring (Component view) ---
    pub_serveOrchestrator -> pub_arbitration "Direct vs programmatic decision"
    pub_serveOrchestrator -> pub_directMatcher "Candidate direct line items"
    pub_serveOrchestrator -> pub_sspClient "Programmatic auction"
    pub_serveOrchestrator -> pub_prebidFanout "Header-bidding demand"
    pub_serveOrchestrator -> pub_formatHandlers "Render winner per format"
    pub_serveOrchestrator -> pub_eventPub "DirectWin / PrebidOutboundWin / ServeNoFill"
    pub_arbitration -> pub_pacing "Guaranteed behind-pace check"
    pub_directMatcher -> postgres "Warm-load publisher line items"
    pub_pacing -> redis "Per-line-item delivery actuals"
    pub_sspClient -> ssp "Run programmatic auction (HTTP)"
    pub_prebidFanout -> prebid "OpenRTB fan-out (HTTP)"
    pub_formatHandlers -> adserver "Render direct-sold creative (HTTP)"
    pub_eventPub -> nats "Publish serve/win events"

    # --- Reporting + Billing component wiring (Component view) ---
    rep_natsConsumer -> nats "Consume business events"
    rep_natsConsumer -> rep_analyticsWriter "Insert events"
    rep_natsConsumer -> rep_billingEngine "Spend events"
    rep_analyticsWriter -> clickhouse "Write hot events + serve reads"
    rep_analyticsWriter -> storage "Read old data from Parquet lake (s3)"
    rep_billingEngine -> tigerbeetle "Reserve / settle ledger"
    rep_billingEngine -> rep_balanceSink "Trigger drawdown on settle"
    rep_balanceSink -> postgres "Debit advertiser_balances"
    rep_balanceSink -> nats "balance-depleted + cache-invalidate"
    rep_pacingPub -> rep_billingEngine "Read committed spend"
    rep_pacingPub -> postgres "Hydrate committed_spend"
    rep_pacingPub -> nats "campaign_spend_snapshot"
    rep_queryEngine -> rep_analyticsWriter "Route query reads"
    rep_exportEngine -> rep_analyticsWriter "Export hot tables"
    rep_exportEngine -> storage "Hourly Parquet export"
    rep_traceInspector -> rep_analyticsWriter "Read trace events"
    gateway -> rep_queryEngine "Report queries (HTTP)"
    gateway -> rep_traceInspector "Trace timeline (HTTP)"
    batchConductor -> rep_exportEngine "Trigger hourly export (HTTP)"

    # --- Tracker component wiring (Component view) ---
    enduser -> trk_beacons "Impression / click / view beacons"
    enduser -> trk_retargetPixel "Site-visit pixel"
    trk_beacons -> trk_hmacValidator "Validate signature"
    trk_beacons -> trk_fraudScorer "Score traffic quality"
    trk_beacons -> trk_dedup "First-seen gate"
    trk_beacons -> trk_eventPub "Publish event"
    trk_beacons -> trk_ara "Register ARA trigger (conversion)"
    trk_fraudScorer -> trk_blocklistCache "Read DB IP/UA blocks"
    trk_blocklistCache -> postgres "Poll fraud_blocklists"
    trk_blocklistCache -> nats "Cache-invalidate (fraud-rules)"
    trk_retargetPixel -> trk_eventPub "Publish site-visit + identity"
    trk_dedup -> redis "SetNX dedup keys"
    trk_ara -> postgres "ara_sources / ara_reports"
    trk_eventPub -> nats "Impression/click/conversion/view + identity.observed"
    trk_eventPub -> storage "Event spool (NATS-stall fallback)"

    # --- Pipeline component wiring (Component view) ---
    pipe_dropzonePoller -> storage "List onboarding bucket + move files"
    pipe_dropzonePoller -> postgres "Enqueue audience_ingest_jobs"
    pipe_dropzonePoller -> smtp "Ingest completion email"
    pipe_ingestWorker -> postgres "Claim jobs (SKIP LOCKED + lease)"
    pipe_ingestWorker -> pipe_ingestProcessor "Process claimed file"
    pipe_ingestProcessor -> pipe_fileProcessor "Decode / validate / normalise"
    pipe_ingestProcessor -> storage "Read staged file, quarantine rejects"
    pipe_ingestProcessor -> postgres "AddMembers / UpsertSegment"
    pipe_ingestProcessor -> nats "Publish ProfileSignalEvent"
    pipe_membershipWriter -> postgres "Drain audience_membership_changelog"
    pipe_membershipWriter -> redis "SADD/SREM audience sets + reconcile"

    # --- Identity Consumer component wiring (Component view) ---
    idc_natsSub -> nats "Consume identity.observed"
    idc_natsSub -> idc_observer "Enqueue observations"
    idc_observer -> idc_determLinker "Co-observed edges (conf 1.0)"
    idc_observer -> idc_probMatcher "Fingerprint matching"
    idc_observer -> idc_edgeWriter "Batch flush"
    idc_probMatcher -> idc_fpStore "Observe(fp, id)"
    idc_fpStore -> redis "Fingerprint buckets (TTL)"
    idc_edgeWriter -> postgres "Upsert identity_graph edges"

    # --- Audience RT component wiring (Component view) ---
    art_behaviourConsumer -> nats "Consume behaviour.observed"
    art_conversionConsumer -> nats "Consume events.conversion"
    art_behaviourConsumer -> art_enrollEngine "Site-visit → enroll"
    art_conversionConsumer -> art_suppressionEngine "Purchase → suppress"
    art_enrollEngine -> art_ruleMatcher "Evaluate single-visit rule"
    art_enrollEngine -> art_productViews "Record viewed SKUs"
    art_enrollEngine -> art_membershipUpsert "AddMembers (TTL)"
    art_suppressionEngine -> art_membershipUpsert "RemoveMember"
    art_suppressionEngine -> art_productViews "Per-SKU burn / cart-clear"
    art_enrollEngine -> nats "Publish retargeting.enrolled"
    art_membershipUpsert -> postgres "audience_segment_members (+ changelog trigger)"
    art_productViews -> postgres "retargeting_product_views / suppressions"
    art_ruleMatcher -> postgres "Read audience_segments rules"

    # --- Report Runner component wiring (Component view) ---
    rr_scheduleEnqueuer -> rr_scopeResolver "Resolve tenant filters"
    rr_scheduleEnqueuer -> rr_jobQueue "Enqueue due reports"
    rr_scheduleEnqueuer -> postgres "Load scheduled_reports"
    rr_jobQueue -> postgres "Claim jobs (SKIP LOCKED + lease)"
    rr_jobQueue -> rr_renderer "Render claimed job"
    rr_jobQueue -> rr_exportZip "Build account-export zip"
    rr_staleSweeper -> rr_jobQueue "Reclaim lapsed leases"
    rr_scopeResolver -> postgres "Account type / publisher ids"
    rr_renderer -> rr_reportingClient "Query analytics data"
    rr_renderer -> rr_artifactStore "Upload CSV/JSON/Parquet"
    rr_renderer -> rr_emailNotifier "Send download link"
    rr_exportZip -> postgres "Read tenant data (scoped tx)"
    rr_exportZip -> rr_artifactStore "Upload export zip"
    rr_reportingClient -> reporting "Query API (HTTP)"
    rr_artifactStore -> storage "adtech-reports bucket"
    rr_emailNotifier -> smtp "Report download link"

    # --- SSAI component wiring (Component view) ---
    ssai_manifestParser -> ssai_stitcher "Break spans → fill loop"
    ssai_stitcher -> ssai_auctionCaller "Fill break via auction"
    ssai_stitcher -> ssai_conditioningClient "Fetch conditioned ad"
    ssai_stitcher -> ssai_beaconSigner "Sign stitched-segment beacons"
    ssai_stitcher -> ssai_freqCapRecorder "Record stitch impression"
    ssai_auctionCaller -> ssai_beaconSigner "Build beacons from winner"
    ssai_segBeaconHandler -> ssai_beaconSigner "Fire pre-signed beacons"
    ssai_auctionCaller -> ssp "Run per-break auction (HTTP)"
    ssai_conditioningClient -> transcoder "Condition ad (cache_only, HTTP)"
    ssai_conditioningClient -> storage "Read conditioned ad cache"
    ssai_freqCapRecorder -> adserver "RECORD freq cap (HTTP)"
    ssai_segBeaconHandler -> tracker "Fire signed impression/quartile beacons"

    # --- Transcoder component wiring (Component view) ---
    tx_httpHandler -> tx_cacheLookup "Query cache_only"
    tx_httpHandler -> tx_ffmpegEngine "Transcode on miss"
    tx_ffmpegEngine -> tx_abrRenditioner "Profile → ffmpeg args"
    tx_ffmpegEngine -> tx_segmentWriter "Upload conditioned segments"
    tx_cacheLookup -> tx_segmentWriter "Read cached playlist/segments"
    tx_cacheLookup -> storage "Exists lookup"
    tx_segmentWriter -> storage "Put playlist + segments"
    ssai -> tx_httpHandler "Condition ad (HTTP)"

    # --- Webhooks component wiring (Component view) ---
    wh_natsConsumer -> nats "Consume account-scoped events"
    wh_natsConsumer -> wh_eventRouter "Extract account + route subject"
    wh_eventRouter -> wh_dispatcher "Dispatch event"
    wh_dispatcher -> wh_subscriptionStore "Lookup active subscriptions"
    wh_dispatcher -> wh_httpDelivery "Deliver to each endpoint"
    wh_httpDelivery -> wh_payloadSigner "Sign envelope (HMAC)"
    wh_httpDelivery -> wh_deliveryLog "Record attempt"
    wh_subscriptionStore -> postgres "Read webhooks (RLS)"
    wh_deliveryLog -> postgres "Write webhook_deliveries"

    # --- Notifications component wiring (Component view) ---
    ntf_natsConsumer -> nats "Consume account-scoped events"
    ntf_natsConsumer -> ntf_eventMapper "Translate to notification"
    ntf_eventMapper -> ntf_postgresWriter "Persist notification"
    ntf_postgresWriter -> postgres "Insert notifications (RLS)"
    gateway -> ntf_postgresWriter "Read bell: list/unread/mark-read"

    # --- Gateway component wiring (Component view) ---
    gw_restHandlers -> gw_authJwt "Require auth"
    gw_restHandlers -> gw_rbacAuthz "Permission check"
    gw_portalRender -> gw_authJwt "Validate session"
    gw_portalRender -> gw_rbacAuthz "Filter nav by permission"
    gw_ssoHandler -> gw_sessionMgr "Mint session cookie"
    gw_ssoHandler -> gw_rbacAuthz "JIT least-privilege role"
    gw_ssoHandler -> oidc "Auth-code + PKCE / id_token verify"
    gw_authJwt -> gw_sessionMgr "Revocation check"
    gw_reverseProxy -> gw_authJwt "Claims → identity headers"
    gw_authJwt -> postgres "User / team lookup"
    gw_ssoHandler -> postgres "SSO config + JIT team_members"
    gw_sessionMgr -> redis "Revocation checkpoint"
    gw_restHandlers -> postgres "Tenant-scoped CRUD"
    gw_restHandlers -> redis "API-key / rate-limit"
    gw_restHandlers -> smtp "Ingest completion email"
    gw_reverseProxy -> dsp "Campaign CRUD (gRPC)"
    gw_reverseProxy -> adserver "Creative CRUD (gRPC)"
    gw_reverseProxy -> ssp "Inventory CRUD (gRPC)"
    gw_reverseProxy -> reporting "Report / trace queries (HTTP)"

    # --- Batch Conductor component wiring (Component view) ---
    bc_checkpoint -> pipeline "Probe /readyz"
    bc_checkpoint -> reporting "Probe /readyz"
    bc_checkpoint -> bc_rollupRunner "Then run (if ready)"
    bc_rollupRunner -> reporting "Run rollup tiers (HTTP)"
    bc_rollupRunner -> bc_exportStep "Then export"
    bc_exportStep -> reporting "Trigger Parquet export (HTTP)"
    bc_exportStep -> bc_profileBuilder "Then build profiles"
    bc_profileBuilder -> postgres "Write audience memberships"
    bc_profileBuilder -> clickhouse "Behaviour signal reads"
    bc_profileBuilder -> storage "Lake reads (fallback)"
    bc_profileBuilder -> bc_privacyDelete "Then purge"
    bc_privacyDelete -> postgres "Delete opted-out users"
    bc_privacyDelete -> bc_privacyVerify "Then verify"
    bc_privacyVerify -> postgres "Residual-PII audit"
    bc_recorder -> postgres "batch_runs rows"
    bc_recorder -> nats "run_completed announcement"
  }

  views {
    systemContext adtech "Context" "The platform, its users, and the external systems it talks to." {
      include *
      autolayout lr
    }

    container adtech "Containers" "Services + datastores and the serving / event / money flows between them." {
      include *
      autolayout lr
    }

    component dsp "DSP-Components" "Inside the DSP: the hot-path bid loop and its in-process caches (the eligibility + shading engine)." {
      include *
      autolayout lr
    }

    component ssp "SSP-Components" "Inside the SSP: builds the bid request and resolves + stamps the user's public audience segments (consent-gated) — the sell-side enrichment the DSPs bid against." {
      include *
      autolayout lr
    }

    component exchange "Exchange-Components" "Inside the exchange: deal matching, SmartRouter DSP fan-out, the auction, and the win/loss notices that carry clear_price back for shading." {
      include *
      autolayout lr
    }

    component adserver "AdServer-Components" "Inside the ad server: creative decisioning + render, frequency capping, HMAC-signed tracking macros, and the creative bandit." {
      include *
      autolayout lr
    }

    component pubAdserver "PubAdServer-Components" "Inside the publisher ad server: direct-sold vs programmatic arbitration per slot, Prebid fan-out, SSP demand, and guaranteed-delivery pacing." {
      include *
      autolayout lr
    }

    component reporting "Reporting-Components" "Inside reporting + billing: the NATS→ClickHouse write path, the in-process reserve/settle billing engine + balance drawdown, pacing snapshots, the query/trace API, and the hourly Parquet export." {
      include *
      autolayout lr
    }

    component tracker "Tracker-Components" "Inside the tracker: beacon handlers behind the signature → fraud → dedup gate, the event publisher, the retargeting pixel + identity bridge, and the ARA overlay." {
      include *
      autolayout lr
    }

    component gateway "Gateway-Components" "Inside the gateway: auth (JWT/SSO/API-key), RBAC, the HTMX portals, the REST API, and the identity-injecting reverse proxy to the internal services." {
      include *
      autolayout lr
    }

    component pipeline "Pipeline-Components" "Inside the pipeline: the onboarding drop-zone poller + ingest worker/processor, and THE single audience-membership cache writer that drains the changelog to Redis." {
      include *
      autolayout lr
    }

    component identityConsumer "IdentityConsumer-Components" "Inside the identity consumer: the observed-identity NATS consumer, the batching observer, deterministic + probabilistic linkers, the Redis fingerprint store, and the identity_graph edge writer." {
      include *
      autolayout lr
    }

    component audienceRT "AudienceRT-Components" "Inside audience RT: the behaviour + conversion consumers driving real-time enroll / suppression, the segment-rule matcher, SKU memory, and the membership upsert." {
      include *
      autolayout lr
    }

    component reportRunner "ReportRunner-Components" "Inside the report runner: schedule enqueuer + scope resolver, the SKIP-LOCKED job queue + stale-lease sweeper, the CSV/JSON/Parquet renderer, export-zip builder, artifact store, and email notifier." {
      include *
      autolayout lr
    }

    component ssai "SSAI-Components" "Inside the SSAI stitcher: manifest parsing, per-break auction, ad stitching, transcoder conditioning, and the pre-signed segment-beacon + freq-cap-record path." {
      include *
      autolayout lr
    }

    component transcoder "Transcoder-Components" "Inside the transcoder: the condition handler branching cache-first lookup vs the ffmpeg ABR-ladder transcode, writing conditioned segments to object storage." {
      include *
      autolayout lr
    }

    component webhooks "Webhooks-Components" "Inside webhooks: the NATS consumer → event router → dispatcher, the subscription store, and the HMAC-signing HTTP delivery client with its delivery log." {
      include *
      autolayout lr
    }

    component notifications "Notifications-Components" "Inside notifications: the queue-grouped NATS consumer, the event→notification mapper, and the RLS-scoped store writer the gateway reads for the bell." {
      include *
      autolayout lr
    }

    component batchConductor "BatchConductor-Components" "Inside the batch conductor: the completion-ordered chain — checkpoint gate → rollups → Parquet export → profile-builder → privacy delete/verify — with the batch_runs recorder." {
      include *
      autolayout lr
    }

    styles {
      element "Person"    { shape person; background "#08427b"; color "#ffffff" }
      element "Container" { background "#1168bd"; color "#ffffff" }
      element "Component" { background "#4a90d9"; color "#ffffff" }
      element "Datastore" { shape cylinder; background "#438dd5"; color "#ffffff" }
      element "External"  { background "#999999"; color "#ffffff" }
      element "Software System" { background "#1168bd"; color "#ffffff" }
    }
  }
}
