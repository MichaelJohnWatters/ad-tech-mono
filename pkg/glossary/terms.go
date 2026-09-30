package glossary

// terms is the full glossary. Grouped by Category; definitions are plain-English,
// HowWeDoIt notes are grounded in this repo's actual code (verified against the
// services/packages/files named). Where the platform does not implement an
// industry concept, HowWeDoIt says so plainly.
var terms = []Term{
	// ============================================================
	// Marketplace roles
	// ============================================================
	{
		Name:       "SSP (Supply-Side Platform)",
		Category:   CatMarketplace,
		Definition: "The platform that represents publishers (the ad supply). It packages a publisher's available impression as a bid request, sends it into the auction, and returns the winning ad. Its job is to maximise the yield the publisher earns for each impression.",
		HowWeDoIt:  "cmd/ssp (:8084). It manages publisher inventory/placements, mints the trace ID at the top of the request, runs the auction via the exchange, then calls the ad server and returns rendered HTML + pixel URLs (routes.SSPServe /v1/ssp/serve). Auction winner/pricing never leak to the browser.",
	},
	{
		Name:       "DSP (Demand-Side Platform)",
		Category:   CatMarketplace,
		Definition: "The platform that represents advertisers (the ad demand). It receives bid requests, decides whether an advertiser's campaign wants the impression, and submits a bid price. Its job is to buy the right impressions for the lowest effective price.",
		HowWeDoIt:  "cmd/dsp (:8082). It evaluates bid requests against campaigns/budgets/targeting and returns an OpenRTB bid (routes.OpenRTBBid /v1/openrtb/bid). The bid loop is strictly in-process — no per-call network I/O; warm copies are kept fresh by background refreshers (cmd/dsp/refresh.go).",
	},
	{
		Name:       "Ad Exchange",
		Category:   CatMarketplace,
		Definition: "The neutral marketplace that runs the real-time auction. It receives a bid request from the SSP, fans it out to many DSPs, collects their bids, applies deal priority and floors, and picks a winner.",
		HowWeDoIt:  "cmd/exchange (:8081). It runs the auction (routes.OpenRTBAuction /v1/openrtb/auction), fans out to our own DSP (gRPC) and external DSPs (OpenRTB HTTP), and applies deal priority. SmartRouter fan-out routing lives in pkg/optimise (routing.go); winner resolution in pkg/auction.",
	},
	{
		Name:       "Ad Server",
		Category:   CatMarketplace,
		Definition: "The service that actually delivers the winning creative to the browser and generates the tracking URLs (impression/click/view beacons) stitched into it. It turns an auction result into rendered markup.",
		HowWeDoIt:  "cmd/adserver (:8085), routes.AdServe /v1/ad/serve. It serves the creative, signs tracker URLs, runs the creative multi-armed-bandit (pkg/optimise), and enforces the display-path frequency cap. cmd/publisher-adserver (:8088) sits in front for direct-sold arbitration.",
	},
	{
		Name:       "Publisher",
		Category:   CatMarketplace,
		Definition: "The owner of the media (website, app, CTV channel, podcast) where ads appear. They supply inventory and earn a revenue share of what advertisers pay.",
		HowWeDoIt:  "A publisher-type account with its own portal (web/templates/portal/publisher.html): placements, deals, direct-sold line items, quality controls, earnings/payouts. Revenue share is per-publisher (publishers.revshare_config; staff editor at /v1/api/revshare).",
	},
	{
		Name:       "Advertiser",
		Category:   CatMarketplace,
		Definition: "The buyer who wants to show ads. They run campaigns with budgets, creatives, and targeting, and pay for delivered impressions/clicks/conversions.",
		HowWeDoIt:  "An advertiser-type account with its own portal (web/templates/portal/advertiser.html): campaigns, creatives, audiences, conversions, attribution, reports, billing. Campaigns/budgets are owned by the DSP; money is drawn down as prepay or on credit (billing terms).",
	},
	{
		Name:       "Agency",
		Category:   CatMarketplace,
		Definition: "An intermediary that manages advertising on behalf of multiple advertiser clients. Agency staff act on their clients' accounts and see a portfolio roll-up across them.",
		HowWeDoIt:  "An agency-type account. Staff assign advertisers to an agency (/v1/api/agency-accounts); the agency 'acts as' a managed advertiser via the act-as switcher. Agency roles carry agency:read (the Portfolio roll-up view).",
	},
	{
		Name:       "Header Bidding",
		Category:   CatMarketplace,
		Definition: "A publisher-side technique where the page solicits bids from several demand sources in parallel (usually via Prebid) before calling its primary ad server, so more buyers compete and yield rises. The auction happens 'in the header' of the page.",
		HowWeDoIt:  "We expose a Prebid Server-compatible bidder endpoint: external Prebid Server instances POST OpenRTB into routes.PrebidAuction (/v1/prebid/openrtb2/auction) and pkg/prebid/prebid.go applies our floor policy then dispatches through the normal auction. Cookie-sync at routes.PrebidSetUID.",
	},

	// ============================================================
	// Auctions & pricing
	// ============================================================
	{
		Name:       "RTB (Real-Time Bidding)",
		Category:   CatAuctions,
		Definition: "The programmatic model where each individual impression is auctioned in real time (typically <100ms) as the page loads. Buyers bid per-impression rather than buying inventory in bulk up front.",
		HowWeDoIt:  "The whole SSP → exchange → DSP fan-out → win → serve path is RTB. The hot-path iron rule keeps the bid loop free of per-call network I/O so it stays inside the latency budget; internal owned edges use gRPC twins (pkg/grpcx), external boundaries stay OpenRTB HTTP.",
	},
	{
		Name:       "OpenRTB",
		Category:   CatAuctions,
		Definition: "The IAB's standard JSON specification for the bid request/response exchanged between SSPs, exchanges, and DSPs. It defines the shape of the impression, user, device, consent, and bid objects so any two parties can interoperate.",
		HowWeDoIt:  "pkg/openrtb holds our request/response types. Every external bidding boundary (third-party DSPs, Prebid, win/loss) speaks OpenRTB JSON/HTTP; only internal edges we own both ends of use the gRPC twin.",
	},
	{
		Name:       "First-Price Auction",
		Category:   CatAuctions,
		Definition: "An auction where the winning bidder pays exactly what they bid. It is the modern programmatic default (it replaced second-price as the ecosystem moved to header bidding), and it forces buyers to think about bid shading because there is no automatic discount.",
		HowWeDoIt:  "First-price is the platform default: pkg/auction/single.go (SingleWinnerStrategy) sets the clearing price to the winner's own bid when PriceMode is 'first_price'. The clearing price rides the AuctionWinEvent (the single source of truth for cost). Because there's no second-price discount, the DSP shades its bids (pkg/bidshading).",
	},
	{
		Name:       "Second-Price Auction",
		Category:   CatAuctions,
		Definition: "An auction where the winner pays just above the second-highest bid, not their own bid. It dominated early programmatic (it makes truthful bidding optimal) but was largely abandoned when header bidding made a single unified first-price auction cleaner.",
		HowWeDoIt:  "Supported but not the default: pkg/auction/single.go clears at the runner-up's price when PriceMode is 'second_price'. The platform default is first-price, so in practice the winner pays their own bid.",
	},
	{
		Name:       "Bid Shading",
		Category:   CatAuctions,
		Definition: "A buyer-side technique for first-price auctions: instead of bidding your full valuation, you bid the lowest amount likely to still win, so you don't overpay. It restores some of the discount that second-price auctions used to give automatically.",
		HowWeDoIt:  "pkg/bidshading on the DSP path. It records every win (did we overpay?) and loss (what cleared?) per placement, builds a win-rate curve, and shades toward the lowest price that still wins the target share. Read-only staff view at /v1/api/staff/shading (Shading section).",
	},
	{
		Name:       "Bid Floor (Floor Price)",
		Category:   CatAuctions,
		Definition: "The minimum price a publisher will accept for an impression. Bids below the floor are rejected. Floors let publishers protect the value of their inventory and are a core yield-management lever.",
		HowWeDoIt:  "pkg/floors resolves the effective floor (base + device/geo/daypart overrides); pkg/auction/strategy.go filters out bids below it before resolving the winner. The DSP's shading tracker even records 'below floor' as a distinct loss reason (surfaced in the staff Shading table).",
	},
	{
		Name:       "Clearing Price",
		Category:   CatAuctions,
		Definition: "The actual price the winning impression clears at — what the advertiser is billed and (after revenue share) what the publisher earns. In a first-price auction this equals the winning bid.",
		HowWeDoIt:  "The clearing price is carried on the AuctionWinEvent, which is the single source of truth for cost (docs/PLAN.md → 'Single Source of Truth: The AuctionWinEvent'). Publisher payouts sum clearing_price_usd from ClickHouse (cmd/payout-runner).",
	},
	{
		Name:       "Tie-Break",
		Category:   CatAuctions,
		Definition: "The rule that decides the winner when two bids are equal (or when deal priority ties). Deterministic tie-breaks keep auctions fair and reproducible.",
		HowWeDoIt:  "Winner resolution lives in pkg/auction (single.go); deal priority is applied by pkg/deals (Priority()/Match()) in the exchange. Ties fall through the same deterministic ordering used for priority.",
	},

	// ============================================================
	// Deals
	// ============================================================
	{
		Name:       "Deal ID",
		Category:   CatDeals,
		Definition: "An identifier attached to a bid request that signals a pre-negotiated agreement between a specific buyer and seller. When a DSP sees a deal ID it recognises, it can bid under the negotiated terms instead of the open auction.",
		HowWeDoIt:  "Deal matching and priority live in pkg/deals; the exchange evaluates deals before the open auction (debug dump at /debug/exchange/deals).",
	},
	{
		Name:       "Programmatic Guaranteed (PG)",
		Category:   CatDeals,
		Definition: "A deal type where a fixed volume of impressions is committed at a fixed price — guaranteed delivery, no auction competition. It's the programmatic equivalent of a traditional direct-sold buy.",
		HowWeDoIt:  "PG is the highest-priority deal type in pkg/deals (Priority()): PG > Preferred > PMP > Open (docs/PLAN.md → 'Deal Management'). Its guaranteed-delivery sibling on the supply side is direct-sold line items (routes.APIDirectLineItems).",
	},
	{
		Name:       "Preferred Deal",
		Category:   CatDeals,
		Definition: "A one-to-one deal at a fixed price but without a volume guarantee — the buyer gets a first look at the inventory and can take it or pass, ahead of the open auction.",
		HowWeDoIt:  "Second in the exchange's deal priority order (PG > Preferred > PMP > Open — docs/PLAN.md → 'Deal Management'; pkg/deals).",
	},
	{
		Name:       "PMP (Private Marketplace)",
		Category:   CatDeals,
		Definition: "An invitation-only auction: a curated set of buyers compete for a publisher's inventory at an agreed floor, but still by auction rather than at a fixed price. It sits between preferred deals and the fully open exchange.",
		HowWeDoIt:  "Third in the deal priority order, above open auction (docs/PLAN.md → 'Deal Management'; pkg/deals).",
	},
	{
		Name:       "Open Auction (Open Exchange)",
		Category:   CatDeals,
		Definition: "The fully open, non-deal marketplace where any eligible buyer can bid on any impression. It has the lowest priority — deals always get first crack at the impression before it falls through to open.",
		HowWeDoIt:  "The fallthrough tier in the exchange after all deal tiers (PG > Preferred > PMP > Open — docs/PLAN.md → 'Deal Management').",
	},
	{
		Name:       "Direct-Sold (Guaranteed / House / Sponsorship)",
		Category:   CatDeals,
		Definition: "Inventory sold directly by the publisher's sales team rather than via auction: guaranteed line items (committed impressions), sponsorships (owning a placement for a period), and house ads (the publisher's own promos that fill unsold inventory).",
		HowWeDoIt:  "cmd/publisher-adserver runs the arbitration ladder (sponsorship → guaranteed → programmatic fallthrough → house) BEFORE any programmatic auction (pkg/publisheradserver; routes.APIDirectLineItems). House ads are the platform's fallback creatives served on a no-bid (staff editor /v1/api/house-ads).",
	},

	// ============================================================
	// Buying models
	// ============================================================
	{
		Name:       "CPM (Cost Per Mille)",
		Category:   CatBuyingModels,
		Definition: "The price per thousand impressions — the base currency of display/video buying. A $2 CPM means the advertiser pays $2 for every 1,000 times the ad is shown.",
		HowWeDoIt:  "pkg/billing/billing.go (BidCPM). The billable event is the impression: spend books on the impression (not the auction win), drawing the winning advertiser's balance down by clearing CPM ÷ 1000 (see the staff 'Billing / Money Flow' demo).",
	},
	{
		Name:       "CPC (Cost Per Click)",
		Category:   CatBuyingModels,
		Definition: "The advertiser pays per click, not per impression. The platform must reserve budget at impression time and only settle (charge) when the click actually happens.",
		HowWeDoIt:  "pkg/billing/billing.go (BidCPC) via the reserve/settle model (SpendEvent/ProcessEvent/SettleByTrace; docs/PLAN.md → 'How Billing Models Interact with AuctionWinEvent'): reserve on the win, settle on the tracked click (routes.TrackerClick /v1/t/click).",
	},
	{
		Name:       "CPA (Cost Per Action / Acquisition)",
		Category:   CatBuyingModels,
		Definition: "The advertiser pays only when a downstream conversion occurs (a purchase, signup, install). It shifts performance risk to the platform, which must attribute the conversion back to the ad.",
		HowWeDoIt:  "pkg/billing/billing.go (BidCPA) reserve/settle, settled on an attributed conversion. Conversions arrive via the signed postback routes.TrackerConversion (/v1/t/conv), validated per-advertiser by HMAC key so no advertiser can forge another's conversion (pkg/attribution; migration 071).",
	},
	{
		Name:       "vCPM (Viewable CPM)",
		Category:   CatBuyingModels,
		Definition: "CPM billed only on impressions that were actually viewable (per the IAB standard: e.g. 50% of pixels in view for 1 second). It rewards quality placements over hidden/below-the-fold ads.",
		HowWeDoIt:  "pkg/billing/billing.go (BidVCPM): reserve on the impression, settle on the viewability beacon (routes.TrackerView /v1/t/view). Viewability is self-measured client-side and fired as a signed beacon (see cmd/viewabilitysmoke).",
	},
	{
		Name:       "CPCV (Cost Per Completed View)",
		Category:   CatBuyingModels,
		Definition: "A video model where the advertiser pays only when the viewer watches the ad to completion (the 100% quartile), not just when it starts. Common for skippable and CTV video.",
		HowWeDoIt:  "pkg/billing/billing.go (BidCPCV): video/audio quartile beacons flow through routes.TrackerVideo / routes.TrackerAudio; the 'complete' quartile is the settle signal (media events queryable via /debug/media_events).",
	},
	{
		Name:       "CPI (Cost Per Install)",
		Category:   CatBuyingModels,
		Definition: "A mobile model where the advertiser pays per app install attributed to the ad. It's a specialised CPA where the action is an app install postback from an MMP (mobile measurement partner).",
		HowWeDoIt:  "Industry concept — the platform models it as a CPA-style conversion (a named conversion type settled via the signed /v1/t/conv postback); there is no dedicated MMP/install-SDK integration.",
	},

	// ============================================================
	// Budget & delivery
	// ============================================================
	{
		Name:       "Pacing",
		Category:   CatDelivery,
		Definition: "The technique of spreading a campaign's budget over its flight so it doesn't spend out in the first hour. Common modes: even (spread evenly across the flight), ASAP (spend as fast as possible), and front-loaded (spend more early).",
		HowWeDoIt:  "pkg/pacing (ModeEven / ModeASAP / ModeFrontLoaded, ShouldBid) computes the spend curve; the DSP throttles bidding against it. Across replicas, a shared additive Redis committed-counter with store reconcile keeps pacing consistent (opt-in reporting.shared_pacing_counter).",
	},
	{
		Name:       "Flight",
		Category:   CatDelivery,
		Definition: "The scheduled run window of a campaign or line item — its start and end dates. Delivery and pacing are calculated relative to the flight.",
		HowWeDoIt:  "IO flight-date transitions (draft→active / active→ended) run daily with a line-item cascade in cmd/dayboundary, which publishes campaign state events + cache invalidate.",
	},
	{
		Name:       "Daily vs Lifetime Budget",
		Category:   CatDelivery,
		Definition: "A daily budget caps spend within a single day; a lifetime budget caps total spend across the whole flight. Pacing has to respect both simultaneously.",
		HowWeDoIt:  "The DSP tracks a per-campaign daily spend counter in Redis (dsp:budget:{day}:{cid}:spent, inspectable via /debug/budget); the billing engine reconciles committed spend and publishes snapshots the DSP pacing gate re-reads.",
	},
	{
		Name:       "Frequency Cap",
		Category:   CatDelivery,
		Definition: "A limit on how many times a given user (or household) sees a specific ad within a time window — to avoid annoying repetition and wasted spend. Enforced per-user, per-campaign, or per-household.",
		HowWeDoIt:  "cmd/adserver/freqcap.go (FreqCap.AllowAndRecord) enforces the display-path cap per household using HMAC(salt, IP) as the household key (Redis keys adserver:freqcap:*). Video/audio caps count stitches, not serve decisions (a PEEK/RECORD split in cmd/ssai). Blocks are observable via /debug/freq_cap_blocks. NOTE: only the display path hits this cap — video/native/audio bypass it.",
	},
	{
		Name:       "Ad Pod",
		Category:   CatDelivery,
		Definition: "A sequence of ad slots played back-to-back in a single break — the digital equivalent of a TV commercial break. Common in CTV/long-form video; each slot may be a separate auction with competitive-separation rules.",
		HowWeDoIt:  "Long-form video breaks are expressed as VMAP schedules where each break's AdSource points at a fresh VAST — separate, independent auctions per slot (routes.PublisherAdServeVMAP /v1/pubad/video/vmap).",
	},

	// ============================================================
	// Creative & formats
	// ============================================================
	{
		Name:       "Display / Banner",
		Category:   CatCreative,
		Definition: "The classic rectangular image or HTML ad shown on a web page or in an app (e.g. 300x250, 728x90). The simplest and most common format.",
		HowWeDoIt:  "The default serve path: the ad server renders the winning display creative to HTML with signed trackers; it's the only path that goes through the household frequency cap.",
	},
	{
		Name:       "Native Ad",
		Category:   CatCreative,
		Definition: "An ad whose layout matches the surrounding content (title, image, description, sponsor) so it feels in-context rather than a bolted-on banner. Defined by the OpenRTB Native spec.",
		HowWeDoIt:  "routes.PublisherAdServeNative (/v1/pubad/native) renders an OpenRTB Native 1.2 winner into an HTML fragment with signed impression/click trackers (SSP channel=native).",
	},
	{
		Name:       "Video: Instream vs Outstream",
		Category:   CatCreative,
		Definition: "Instream video plays inside actual video content (pre/mid/post-roll around a clip you're watching). Outstream video plays outside a video player — e.g. an autoplay unit that appears within an article's text.",
		HowWeDoIt:  "Instream is the VAST/VMAP path (routes.PublisherAdServeVAST / …VMAP), where players fetch XML and parse out the media + tracker URLs. SSP channel=video drives the per-break auctions.",
	},
	{
		Name:       "Audio Ad / DAAST",
		Category:   CatCreative,
		Definition: "An audio-only ad for podcasts and streaming radio. DAAST was the IAB's audio ad spec (since folded into VAST 4.x, which carries an audio MediaFile with no width/height).",
		HowWeDoIt:  "routes.PublisherAdServeAudio (/v1/pubad/audio) returns a VAST 4.2 doc carrying an audio/mpeg MediaFile; quartile beacons route through routes.TrackerAudio (/v1/t/audio). Same auction shape as video (SSP channel=audio).",
	},
	{
		Name:       "VAST (Video Ad Serving Template)",
		Category:   CatCreative,
		Definition: "The IAB XML spec a video player fetches to learn what ad to play: the media file URL plus the impression and quartile tracking beacons. It's how video ads are delivered independent of the player.",
		HowWeDoIt:  "pkg/vast builds the document; routes.PublisherAdServeVAST (/v1/pubad/video/vast) returns VAST 4.2 XML; IMA SDK / video.js / hls.js fetch and parse it. Same auction under the hood as the display serve — only the response is XML.",
	},
	{
		Name:       "VMAP (Video Multiple Ad Playlist)",
		Category:   CatCreative,
		Definition: "An IAB XML schedule describing WHEN ad breaks occur in a piece of content (pre-roll, mid-roll, post-roll) and where to fetch each break's ads. It wraps VAST — one VMAP, many VAST fetches.",
		HowWeDoIt:  "pkg/vmap builds the schedule; routes.PublisherAdServeVMAP (/v1/pubad/video/vmap) returns a VMAP 1.0 schedule whose breaks' AdSource AdTagURIs point back at the VAST endpoint, so the player fetches a fresh independent VAST (independent auction/winner) per break.",
	},
	{
		Name:       "VPAID",
		Category:   CatCreative,
		Definition: "An older IAB spec for interactive/executable video ads (the ad runs its own JavaScript for interactivity and verification). Largely deprecated in favour of VAST 4.x + OMID because it was heavy and a security risk.",
		HowWeDoIt:  "Industry background — not implemented. The video path serves VAST 4.2 (declarative XML), not VPAID executable units.",
	},
	{
		Name:       "SSAI (Server-Side Ad Insertion)",
		Category:   CatCreative,
		Definition: "Stitching ads directly into the content video/audio stream on the server, so the client just plays one continuous manifest. It defeats ad blockers and gives a seamless TV-like experience, at the cost of client-side interactivity.",
		HowWeDoIt:  "cmd/ssai (:8093, main.go) returns an HLS/DASH manifest with ads stitched in (routes.SSAIManifest / …MPD), runs a per-break auction, and fires beacons SERVER-SIDE on segment fetch (HMAC-signed, routes.SSAISegment). cmd/transcoder conditions winning ads to be byte-compatible with the content stream.",
	},
	{
		Name:       "CTV (Connected TV)",
		Category:   CatCreative,
		Definition: "Ads delivered to internet-connected TVs and streaming devices. Premium, largely non-skippable, sold in pods, and usually delivered via SSAI for a broadcast-quality experience.",
		HowWeDoIt:  "Modelled as the video channel with pod/VMAP break scheduling and SSAI stitching (cmd/ssai). Phase 9 built the video/CTV core; simulator personas exercise the CTV path.",
	},
	{
		Name:       "DOOH (Digital Out-Of-Home)",
		Category:   CatCreative,
		Definition: "Programmatic buying of digital billboards, transit screens, and other physical-world displays. Impressions are modelled probabilistically (estimated audience per play) rather than one-user-one-view.",
		HowWeDoIt:  "Shipped as an MVP channel; its auction is the time-slot strategy in pkg/auction/timeslot.go (Phase 9: all 5 auction strategies real, multi-winner money e2e-green).",
	},
	{
		Name:       "In-Game Advertising",
		Category:   CatCreative,
		Definition: "Ads placed inside video games — as in-world surfaces (billboards in the game scene) or interstitial/rewarded units. A growing channel with its own measurement and placement rules.",
		HowWeDoIt:  "Shipped as an MVP channel; its auction is the batch strategy in pkg/auction/batch.go (Phase 9: DOOH/RETAIL/IN-GAME MVPs shipped).",
	},
	{
		Name:       "Retail Media",
		Category:   CatCreative,
		Definition: "Advertising on a retailer's own digital properties (site, app, in-store screens) using the retailer's first-party purchase data. One of the fastest-growing ad channels because the data is closed-loop to sales.",
		HowWeDoIt:  "Shipped as an MVP channel; its auction is the relevance-weighted strategy in pkg/auction/relevance.go (Phase 9: DOOH/RETAIL/IN-GAME MVPs shipped).",
	},
	{
		Name:       "Interstitial",
		Category:   CatCreative,
		Definition: "A full-screen ad shown at a natural transition point (between app screens or game levels). High-impact but interruptive.",
		HowWeDoIt:  "Industry format — served through the standard display/video serve paths; there is no interstitial-specific enforcement beyond the channel/format the placement declares.",
	},
	{
		Name:       "Rewarded Ad",
		Category:   CatCreative,
		Definition: "An opt-in ad (usually in games/apps) where the user chooses to watch in exchange for an in-app reward. High completion rates because the user consents to watch.",
		HowWeDoIt:  "Industry format — not modelled as a distinct rewarded flow (no reward-grant callback); it would ride the video serve path.",
	},
	{
		Name:       "Dynamic Product Ads (DPA)",
		Category:   CatCreative,
		Definition: "Ads whose creative is assembled at render time from a product catalog, personalised to show the exact products a user viewed or carted (plus complementary cross-sells). The 'chase' ad that follows you after browsing a store.",
		HowWeDoIt:  "Shipped epic: a product catalog feed rides the unified ingest (kind=product, routes.APIProducts), a SKU-aware pixel (/v1/t/rt) records views, and the render-time dynamic_product creative (cmd/adserver/dynamic_products.go) renders a Go template × carted SKUs with a static fallback. Suppresses purchased SKUs and rotates to complements. See docs/DYNAMIC-PRODUCT-ADS.md.",
	},

	// ============================================================
	// Identity & privacy
	// ============================================================
	{
		Name:       "Identity Graph",
		Category:   CatIdentity,
		Definition: "A store of links between the many identifiers that belong to one person or household (cookies, device IDs, hashed emails, CTV IDs). It lets the platform recognise the same user across devices for frequency capping, attribution, and targeting.",
		HowWeDoIt:  "pkg/identity/identity.go holds the graph; cmd/identity-consumer builds it by consuming adtech.identity.observed from the SSP, batching/deduping and writing deterministic + probabilistic edges. Multi-replica-safe when fingerprint buckets are Redis-backed.",
	},
	{
		Name:       "UID2 (Unified ID 2.0)",
		Category:   CatIdentity,
		Definition: "An open, privacy-forward identity standard built on hashed+salted email/phone, designed as a cookieless successor for cross-site addressability with user consent.",
		HowWeDoIt:  "pkg/openrtb/eid.go parses UID2 from the request's extended IDs (UID2Source, UID2From/UID2EID) and it's carried into the identity graph as an edge. Consent still gates whether it's used for personalisation (pkg/privacy).",
	},
	{
		Name:       "Consent / GDPR / TCF / GPP",
		Category:   CatIdentity,
		Definition: "The user-permission signals that must flow through the ad chain. GDPR (EU law) requires a lawful basis; TCF and GPP are the IAB frameworks that encode consent strings passed in the bid request. Without consent, no user-level targeting is allowed.",
		HowWeDoIt:  "Consent flows through the whole chain and is evaluated by pkg/privacy/consent.go (privacy.Evaluate() over a Signals struct → .Personalise). The SSP gates user-level signal emission (identity/behaviour/segments) on consent before expressing them to bidders; personalisation requires it.",
	},
	{
		Name:       "Opt-Out / Data Deletion",
		Category:   CatIdentity,
		Definition: "A user's right to withdraw from personalisation, tracking, or to have their data erased entirely (GDPR/CCPA). Typically tiered: no personalisation, no tracking, full deletion.",
		HowWeDoIt:  "3-level opt-out (routes.APIPrivacyOptOut /v1/api/privacy/optout): L1 no personalisation, L2 no tracking, L3 full GDPR/CCPA deletion. Enforced platform-wide within a second (DSP no-bid + segment strip, tracker reject). L3 purge runs in cmd/privacy-delete across Postgres + ClickHouse + Parquet re-export.",
	},
	{
		Name:       "PII Hashing",
		Category:   CatIdentity,
		Definition: "Personally identifiable information (email, phone) must never be transmitted or stored in the clear — it's hashed (usually SHA-256, often salted) client-side so identifiers can be matched without exposing the raw value.",
		HowWeDoIt:  "Project rule (root CLAUDE.md → Privacy; pkg/privacy): never store raw PII — hash client-side before transmission. Audience onboarding matches on hashed identifiers (pkg/audience), and uploads can be PGP-encrypted to the platform's public key before transit (routes.APIAudiencePGPKey). There is no single hashing module — hashing happens at the ingest/onboarding boundary.",
	},
	{
		Name:       "Data Residency",
		Category:   CatIdentity,
		Definition: "The requirement that an account's data stay in a specific geographic region (e.g. EU data stays in the EU). Both the control plane (mutations) and the data plane (what signals are emitted) must respect the region.",
		HowWeDoIt:  "MVP shipped (migration 103 accounts.residency_region + platform.region): the JWT carries ResidencyRegion and middleware.Auth 403s out-of-region mutations (staff-exempt). On the data plane pkg/privacy/residency.go (AllowsResidency / AllowsUserData) gates the SSP's identity/behaviour/segment emission on regs.ext.data_residency vs the account's home region. Single-region enforce; multi-region infra deferred.",
	},

	// ============================================================
	// Supply chain & fraud
	// ============================================================
	{
		Name:       "schain (SupplyChain Object)",
		Category:   CatSupplyChain,
		Definition: "An OpenRTB object that records every intermediary an impression passed through, from the publisher to the exchange. It lets buyers verify they're buying legitimate, non-laundered inventory and see who's taking a cut.",
		HowWeDoIt:  "pkg/openrtb/schain.go carries + validates the SupplyChain object (ValidateSChain, SChainOf); the exchange enforces it in cmd/exchange/schain.go so laundered/malformed supply can be rejected.",
	},
	{
		Name:       "ads.txt / app-ads.txt",
		Category:   CatSupplyChain,
		Definition: "Public files publishers host at their domain root listing the sellers authorised to sell their inventory. Buyers check them to reject spoofed/unauthorised supply. app-ads.txt is the mobile-app equivalent (hosted at the developer's domain).",
		HowWeDoIt:  "cmd/adstxt crawls publisher ads.txt into ads_txt_cache and publishes a cache-invalidate on change; enforcement lives in pkg/fraud/adstxt.go. The publisher-facing 'how to authorise us' line is served from routes.APIIntegrationAdsTxt, read from the same exchange config strict mode checks. cmd/appadstxt crawls the app twin (no enforcement consumer yet).",
	},
	{
		Name:       "IVT / Ad Fraud",
		Category:   CatSupplyChain,
		Definition: "Invalid Traffic — impressions or clicks generated by bots, data-center traffic, or spoofing rather than real humans. Fraud detection scores traffic in real time and blocks known-bad sources.",
		HowWeDoIt:  "Real-time checks + scoring live in pkg/fraud on the tracker path (blocklists, ads.txt, scoring). Staff manage blocklists at routes.APIFraudBlocklists; a change invalidates the tracker's warm cache sub-second. Rejections are observable via /debug/tracker_rejections (reason=fraud).",
	},
	{
		Name:       "Viewability",
		Category:   CatSupplyChain,
		Definition: "Whether an ad was actually seen — the IAB standard is 50% of the ad's pixels in view for at least 1 continuous second (2 seconds for video). It separates 'served' from 'seen' and underpins vCPM.",
		HowWeDoIt:  "Measured client-side and reported via a signed beacon to routes.TrackerView (/v1/t/view), recorded by the tracker (cmd/tracker). cmd/viewabilitysmoke drives the demosite video page to self-measure and fire the beacon, asserting the row lands in ClickHouse.",
	},

	// ============================================================
	// Measurement & data
	// ============================================================
	{
		Name:       "Impression",
		Category:   CatMeasurement,
		Definition: "A single instance of an ad being shown. It's the fundamental billable/measured event in display and video (the moment the creative renders and the impression beacon fires).",
		HowWeDoIt:  "Fired to routes.TrackerImpression (/v1/t/imp), the impression is the billable event — spend books here, not on the auction win. The tracker publishes it to NATS; reporting consumes it into ClickHouse exactly-once (Nats-Msg-Id + business-key dedup).",
	},
	{
		Name:       "Click",
		Category:   CatMeasurement,
		Definition: "A user interaction with the ad, redirecting to the advertiser's landing page. The billable event for CPC and a key input to attribution.",
		HowWeDoIt:  "routes.TrackerClick (/v1/t/click). Click-through attribution rides a signed ctid so a conversion can be tied back to the click without trusting client-supplied fields (pkg/attribution).",
	},
	{
		Name:       "Conversion",
		Category:   CatMeasurement,
		Definition: "A valuable post-click/post-view action on the advertiser's site — a purchase, signup, or install. The billable event for CPA and the goal of performance campaigns.",
		HowWeDoIt:  "Advertisers define named conversion types (routes.APIConversions) and fire the signed server-to-server postback routes.TrackerConversion (/v1/t/conv), validated per-advertiser by HMAC key (routes.APIConversionKey) so no advertiser can forge another's conversion.",
	},
	{
		Name:       "Attribution",
		Category:   CatMeasurement,
		Definition: "The process of crediting a conversion back to the ad exposure(s) that drove it. Models include last-click, view-through, and multi-touch (spreading credit across every touchpoint).",
		HowWeDoIt:  "pkg/attribution: click-through via the signed ctid, view-through + cross-device via the identity graph, and multi-touch apportionment (pkg/attribution/apportion.go — last_touch / first_touch / linear / time_decay / position_based). Reporting serves a conversion's touchpoint chain with per-touch credit under a chosen model (routes.ReportingAttribution).",
	},
	{
		Name:       "View-Through Conversion",
		Category:   CatMeasurement,
		Definition: "A conversion credited to an ad the user SAW but didn't click, within a lookback window. It captures the influence of impressions that don't get a click but still move the user.",
		HowWeDoIt:  "Resolved by pkg/attribution using the identity graph to link the impression to the later conversion (cross-device), gated by a confidence floor and consent-aware SSP observation.",
	},
	{
		Name:       "Data Rollups",
		Category:   CatMeasurement,
		Definition: "Pre-aggregating raw event rows into coarser summaries (per-minute → hourly → daily → monthly) so reporting queries stay fast as data grows. Each tier keeps the same totals with far fewer rows.",
		HowWeDoIt:  "A universal rollup framework (docs/PLAN.md → 'Data Rollups'); the batch conductor runs the rollup tiers hourly (cmd/batch-conductor). Reporting auto-picks the coarsest tier that answers a query's range. See the staff 'Rollups' demo.",
	},
	{
		Name:       "Trace ID",
		Category:   CatMeasurement,
		Definition: "A unique ID stamped on a request at entry and carried through every service, log line, and event, so you can reconstruct one ad request's entire journey end-to-end. The backbone of full-transparency debugging.",
		HowWeDoIt:  "Minted at the SSP, propagated via the X-Trace-ID HTTP header and gRPC metadata (pkg/grpcx/trace.go interceptors carry W3C traceparent; pkg/tracing.TraceIDFromContext extracts it), and required in every slog line and NATS message. The portal Trace Inspector (routes.ReportingTrace) reconstructs a request's flow, scoped/redacted per caller.",
	},

	// ============================================================
	// Segments & audience
	// ============================================================
	{
		Name:       "Segment",
		Category:   CatAudience,
		Definition: "A named group of users sharing an attribute or behaviour (e.g. 'auto intenders', 'cart abandoners'). Advertisers target segments; the platform matches a request's user against segment membership at bid time.",
		HowWeDoIt:  "pkg/audience/audience.go holds segment logic; membership lives in audience_segment_members. Membership is pushed to Redis SETs (audience:set:*) via an append-based changelog so the DSP/SSP read it in-process at bid time (SMEMBERS) — no per-call DB hit.",
	},
	{
		Name:       "IAB Taxonomy / segtax",
		Category:   CatAudience,
		Definition: "The IAB's standardised audience/content taxonomy — a shared vocabulary of segment IDs so a segment means the same thing to every buyer. 'segtax' is the OpenRTB field naming which taxonomy a segment ID belongs to (audience taxonomy = 4).",
		HowWeDoIt:  "pkg/taxonomy holds the taxonomy model; IAB labels (migration 062; official TSV via cmd/taxonomy-import) tag segments. Labelled PUBLIC segments ride to external bidders as user.data with segtax=4 (consent-gated at the SSP) and can earn data fees per delivered impression (routes.APIAudienceFee / …/earnings).",
	},
	{
		Name:       "CRM Onboarding",
		Category:   CatAudience,
		Definition: "Uploading an advertiser's own customer list (emails/phones) and matching it — via hashing — to the platform's identifiers so those known customers can be targeted or suppressed. The bridge from offline first-party data to addressable audiences.",
		HowWeDoIt:  "Audience upload creates a segment + bulk-adds members (routes.APIAudiences); files ride the unified ingestion/drop-zone path (cmd/pipeline), match on hashed identifiers, and report a match rate. Providers/mappings/encryption contracts are configurable (ADR 0008/0009).",
	},
	{
		Name:       "Retargeting",
		Category:   CatAudience,
		Definition: "Showing ads to users who already interacted with the advertiser (visited the site, viewed a product, abandoned a cart) and suppressing those who converted. The highest-intent, highest-performing audience type.",
		HowWeDoIt:  "cmd/audience-rt (:8097) enrolls a visitor into the advertiser's retargeting segment on the /v1/t/rt site_visit within SECONDS (vs the hourly batch profile-builder) and suppresses on purchase; core logic in pkg/retargeting/retargeting.go. Single-visit rules only; frequency rules stay batch.",
	},
	{
		Name:       "Clean Room",
		Category:   CatAudience,
		Definition: "A privacy-safe compute environment where two parties (e.g. advertiser + publisher) match and analyse their data together without either seeing the other's raw records — outputs are aggregated and above a minimum count.",
		HowWeDoIt:  "Shipped as clean-room-lite in the Data Marketplace (pkg/marketplace): audience expansion/overlap estimates enforce a minimum-aggregation privacy floor (min 100) so no small-cohort leakage. Full clean-room compute (cmd/cleanroom) is a deferred placeholder (.gitkeep only).",
	},
	{
		Name:       "Data Marketplace",
		Category:   CatAudience,
		Definition: "A marketplace where data owners list audience segments for other accounts to buy and activate, typically monetised as a CPM surcharge on impressions that use the purchased data.",
		HowWeDoIt:  "PLAN Phase 10, complete: list/browse/purchase/grants/estimate (routes.APIMarketplace*), CPM-surcharge settlement (migration 090, reporting-only), and expansion estimates. Only PUBLIC segments are listable; cross-tenant reads go through the platform hatch; bid-time matching needs no change (pkg/marketplace).",
	},
}
