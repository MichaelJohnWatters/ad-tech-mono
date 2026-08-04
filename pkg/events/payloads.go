package events

import "time"

// CurrentSchemaVersion is the version every wire-format event payload
// carries today. Bump when we make a backwards-incompatible change to
// any JSON payload; consumers in cmd/reporting (and the analytics
// mirrors in pkg/store/analytics) branch on this when migration logic
// lands. Today the field is informational only — see
// docs/EVENT_PATHWAY_AUDIT.md "schema_version policy" sub-task for the
// invariant and TestAllEventPayloadsHaveSchemaVersion for the
// enforcement test.
const CurrentSchemaVersion = 1

// AuctionWinEvent is published by the Exchange after an auction completes.
// Single source of truth for cost. Consumed by DSP (budget) and Reporting (billing).
type AuctionWinEvent struct {
	SchemaVersion int       `json:"schema_version"`
	TraceID       string    `json:"trace_id"`
	AuctionID     string    `json:"auction_id"`
	WinnerDSP     string    `json:"winner_dsp"`
	CampaignID    string    `json:"campaign_id"`
	CreativeID    string    `json:"creative_id"`
	PlacementID   string    `json:"placement_id"`
	PublisherID   string    `json:"publisher_id"`
	AdvertiserID  string    `json:"advertiser_id"`
	ClearingPrice float64   `json:"clearing_price"`
	Currency      string    `json:"currency"`
	BidModel      string    `json:"bid_model"`
	DealID        string    `json:"deal_id,omitempty"`
	Channel       string    `json:"channel"`
	Timestamp     time.Time `json:"timestamp"`
}

// DataFeeSegment is one fee-bearing segment that rode a won bid request:
// who owns the data and what it costs (CPM in MICRO-dollars).
type DataFeeSegment struct {
	SegmentID      string `json:"segment_id"`
	OwnerAccountID string `json:"owner_account_id"`
	FeeMicros      int64  `json:"fee_micros"`
}

// DataFeeEvent is published by the SSP when an EXTERNAL bidder wins an
// auction whose bid request carried fee-bearing audience data (public,
// taxonomy-labelled segments stamped as user.data — consent already gated at
// the stamp). It is the attribution record for data monetization: reporting
// holds it (durable, data_fee_pending) until the impression for the trace
// arrives, then accrues the fee to each segment owner net of platform
// margin. Deliberately a separate event — segment owners and fees must never
// ride the bid request itself, which external parties receive verbatim.
type DataFeeEvent struct {
	SchemaVersion int              `json:"schema_version"`
	TraceID       string           `json:"trace_id"`
	PlacementID   string           `json:"placement_id"`
	PublisherID   string           `json:"publisher_id"`
	WinnerSeat    string           `json:"winner_seat"`
	Segments      []DataFeeSegment `json:"segments"`
	ClearingPrice float64          `json:"clearing_price"`
	Timestamp     time.Time        `json:"timestamp"`
}

// CampaignSpendSnapshotEvent is the periodic per-campaign committed-spend
// broadcast from Reporting (billing engine) to every DSP pod. Committed maps a
// campaign id (the line-item UUID) to committed spend in MICRO-DOLLARS (1 USD = 1e6 µ) for the given
// UTC Day, where committed = settled-today + open-reserves. A DSP reconciles
// each of its own campaigns' pacing counters to this value; campaigns absent
// from the map had no billing activity today and are left untouched (so a DSP
// never wipes a local in-flight win counter it hasn't billed yet).
type CampaignSpendSnapshotEvent struct {
	SchemaVersion int              `json:"schema_version"`
	Day           string           `json:"day"` // UTC yyyy-mm-dd the totals belong to
	Currency      string           `json:"currency"`
	Committed     map[string]int64 `json:"committed"` // campaign_id → committed micro-dollars
	Timestamp     time.Time        `json:"timestamp"`
}

// DSPCallEvent records the outcome of one DSP fan-out call in an auction —
// bid received?, price, latency, timeout. The exchange emits one per DSP per
// auction (fire-and-forget) so routing behaviour is analysable historically in
// ClickHouse/Parquet and the SmartRouter can warm-start from it after a
// restart. Win attribution is derived by joining to auction_wins on trace_id
// (kept out of this event so it can be emitted at fan-out time, before the
// winner is known).
type DSPCallEvent struct {
	SchemaVersion int     `json:"schema_version"`
	TraceID       string  `json:"trace_id"`
	AuctionID     string  `json:"auction_id"`
	Channel       string  `json:"channel"`
	DSPEndpoint   string  `json:"dsp_endpoint"`
	BidReceived   bool    `json:"bid_received"`
	BidPriceUSD   float64 `json:"bid_price_usd"`
	LatencyMs     int64   `json:"latency_ms"`
	TimedOut      bool    `json:"timed_out"`
	// NoBidReason carries the DSP's no-bid reason string when it declined for a
	// stated reason (e.g. "adcert_invalid"). Empty when the DSP bid or simply
	// had no matching demand. This is what keeps a DSP-level enforcement block
	// from vanishing into the exchange's aggregated no-bid — it's persisted per
	// DSP and surfaced in the trace timeline.
	NoBidReason string    `json:"no_bid_reason,omitempty"`
	Timestamp   time.Time `json:"timestamp"`
}

// AuctionCompleteEvent includes all bids and timing (for analytics).
type AuctionCompleteEvent struct {
	SchemaVersion int          `json:"schema_version"`
	TraceID       string       `json:"trace_id"`
	PlacementID   string       `json:"placement_id"`
	PublisherID   string       `json:"publisher_id"`
	Channel       string       `json:"channel"`
	NumBids       int          `json:"num_bids"`
	WinnerDSP     string       `json:"winner_dsp,omitempty"`
	ClearingPrice float64      `json:"clearing_price,omitempty"`
	FloorPrice    float64      `json:"floor_price"`
	DurationMs    int64        `json:"duration_ms"`
	Bids          []BidSummary `json:"bids,omitempty"`
	Timestamp     time.Time    `json:"timestamp"`
}

// BidSummary is a single bid in the auction complete event.
type BidSummary struct {
	DSPID      string  `json:"dsp_id"`
	CampaignID string  `json:"campaign_id"`
	Price      float64 `json:"price"`
	Won        bool    `json:"won"`
}

// BudgetDepletedEvent is published by the DSP when a campaign runs out of budget.
type BudgetDepletedEvent struct {
	SchemaVersion int       `json:"schema_version"`
	CampaignID    string    `json:"campaign_id"`
	AccountID     string    `json:"account_id"`
	Budget        float64   `json:"budget"`
	Spent         float64   `json:"spent"`
	Timestamp     time.Time `json:"timestamp"`
}

// BalanceDepletedEvent is published when an advertiser account's prepay
// balance is exhausted — bidding stops platform-wide for that account
// until the next topup (the account-level sibling of BudgetDepletedEvent).
type BalanceDepletedEvent struct {
	SchemaVersion int       `json:"schema_version"`
	AccountID     string    `json:"account_id"`
	Balance       float64   `json:"balance"`
	Timestamp     time.Time `json:"timestamp"`
}

// CampaignStateEvent is published when a campaign changes state.
type CampaignStateEvent struct {
	SchemaVersion int       `json:"schema_version"`
	CampaignID    string    `json:"campaign_id"`
	AccountID     string    `json:"account_id"`
	OldState      string    `json:"old_state"`
	NewState      string    `json:"new_state"`
	Reason        string    `json:"reason,omitempty"`
	Timestamp     time.Time `json:"timestamp"`
}

// OptOutEvent is published when a user opts out.
type OptOutEvent struct {
	SchemaVersion int       `json:"schema_version"`
	UserID        string    `json:"user_id"`
	Level         int       `json:"level"` // 1, 2, or 3
	Source        string    `json:"source"`
	Timestamp     time.Time `json:"timestamp"`
}

// CacheInvalidateEvent tells services to clear their L1 cache for a resource.
type CacheInvalidateEvent struct {
	SchemaVersion int    `json:"schema_version"`
	ResourceType  string `json:"resource_type"` // campaign, placement, creative, dsp-endpoint
	ResourceID    string `json:"resource_id"`
	Action        string `json:"action"` // update, delete
}

// VideoEvent is published by the tracker on /v1/t/video pixel hits.
// EventType is the VAST event name (start, firstQuartile, midpoint,
// thirdQuartile, complete, skip, mute, unmute, …). PositionMs carries
// the playback offset if the player sent one.
type VideoEvent struct {
	SchemaVersion int       `json:"schema_version"`
	TraceID       string    `json:"trace_id"`
	EventType     string    `json:"event_type"`
	PositionMs    int64     `json:"position_ms,omitempty"`
	Duration      int       `json:"duration_seconds,omitempty"`
	Timestamp     time.Time `json:"timestamp"`
}

// AudioEvent — DAAST audio equivalent of VideoEvent published from
// /v1/t/audio.
type AudioEvent struct {
	SchemaVersion int       `json:"schema_version"`
	TraceID       string    `json:"trace_id"`
	EventType     string    `json:"event_type"`
	PositionMs    int64     `json:"position_ms,omitempty"`
	Duration      int       `json:"duration_seconds,omitempty"`
	Timestamp     time.Time `json:"timestamp"`
}

// AdserverRenderFailedEvent is published by cmd/adserver when serve
// can't produce the requested creative. The browser still gets a
// rendered response (default placeholder HTML for unknown_creative,
// or whatever the failure path returns) so failures are invisible to
// the impression-tracking pixel path — reporting needs this signal
// to detect broken creatives.
//
//	Reason: "unknown_creative" | "render_error" | "asset_missing"
//	Detail: free-form context — for unknown_creative this is the
//	        requested creative_id; for render_error it's the
//	        underlying error string.
//
// AdserverFreqCapBlockedEvent fires when the ad server's
// (user, campaign) freq-cap counter is saturated and the serve
// request is suppressed before any creative is rendered. Distinct
// from TrackerRejectedEvent (post-serve drops); this is the
// pre-serve drop signal. Reporting consumes it for ops dashboards
// (alert on suppression-rate change) + advertiser reports
// ("we suppressed N over-cap impressions").
type AdserverFreqCapBlockedEvent struct {
	SchemaVersion int       `json:"schema_version"`
	TraceID       string    `json:"trace_id"`
	UserID        string    `json:"user_id"`
	CampaignID    string    `json:"campaign_id"`
	PlacementID   string    `json:"placement_id,omitempty"`
	PublisherID   string    `json:"publisher_id,omitempty"`
	Timestamp     time.Time `json:"timestamp"`
}

type AdserverRenderFailedEvent struct {
	SchemaVersion int       `json:"schema_version"`
	TraceID       string    `json:"trace_id"`
	CampaignID    string    `json:"campaign_id,omitempty"`
	CreativeID    string    `json:"creative_id,omitempty"`
	PlacementID   string    `json:"placement_id,omitempty"`
	PublisherID   string    `json:"publisher_id,omitempty"`
	Reason        string    `json:"reason"`
	Detail        string    `json:"detail,omitempty"`
	Timestamp     time.Time `json:"timestamp"`
}

// TrackerRejectedEvent is published by cmd/tracker every time a pixel
// request is dropped before recording: invalid HMAC sig (strict mode),
// fraud check blocked, or dedup hit. Negative signal — analytics
// queries that compute true-cost-per-acquisition subtract these from
// the denominator; ops dashboards alert on rate-of-change.
//
//	EventType: "impression" | "click" | "conversion" | "view" |
//	           "video" | "audio"
//	Reason:    "invalid_signature" | "fraud" | "dedup"
//	Detail:    free-form, populated for "fraud" with the underlying
//	           reasons array (joined by comma). Empty otherwise.
type TrackerRejectedEvent struct {
	SchemaVersion int       `json:"schema_version"`
	TraceID       string    `json:"trace_id"`
	EventType     string    `json:"event_type"`
	Reason        string    `json:"reason"`
	Detail        string    `json:"detail,omitempty"`
	Timestamp     time.Time `json:"timestamp"`
}

// ServeNoFillEvent is published by cmd/publisher-adserver when an ad
// request fell through every demand source (no direct line item, no
// programmatic bid, no house creative). Counterpart to AuctionWinEvent —
// required for fill-rate computation. Without it, analytics could see
// served impressions but had no record of misses except via log scraping.
type ServeNoFillEvent struct {
	SchemaVersion int       `json:"schema_version"`
	TraceID       string    `json:"trace_id"`
	PublisherID   string    `json:"publisher_id"`
	PlacementID   string    `json:"placement_id"`
	Reason        string    `json:"reason"` // free-form: which fallthroughs were exhausted
	Timestamp     time.Time `json:"timestamp"`
}

// DirectWinEvent is published by cmd/publisher-adserver every time a
// direct-sold line item (sponsorship/guaranteed/house) wins arbitration
// and gets served. It's the "we just delivered a direct impression"
// signal — the analogue of AuctionWinEvent for the non-programmatic path,
// closing what was a billing-and-analytics blind spot where direct
// serves only surfaced via the tracker pixel with no line-item context.
//
// Reporting consumes this to write to the analytics store and, for tiers
// with a real CPM (sponsorship/guaranteed), accrue spend to the
// publisher line item's owner.
type DirectWinEvent struct {
	SchemaVersion       int       `json:"schema_version"`
	TraceID             string    `json:"trace_id"`
	PublisherLineItemID string    `json:"publisher_line_item_id"`
	PublisherID         string    `json:"publisher_id"`
	PlacementID         string    `json:"placement_id"`
	PriorityTier        string    `json:"priority_tier"` // sponsorship | guaranteed | preferred | house
	DemandSource        string    `json:"demand_source"` // brand name string from the line item
	CreativeID          string    `json:"creative_id"`
	CPM                 float64   `json:"cpm"`
	Currency            string    `json:"currency"`
	Timestamp           time.Time `json:"timestamp"`
}

// RetargetingEnrolledEvent is emitted by audience-rt when a shopper is enrolled
// into an advertiser's real-time retargeting audience. Account-scoped
// (AccountID = the advertiser) so the webhooks dispatcher can deliver it to the
// advertiser's registered endpoints — the hook for an abandoned-cart push.
type RetargetingEnrolledEvent struct {
	SchemaVersion int `json:"schema_version"`
	// TraceID is the originating site_visit's trace, so the webhook delivery is
	// linkable back to the visit that triggered it (and rides the same OTel trace
	// the NATS headers already propagate).
	TraceID    string    `json:"trace_id,omitempty"`
	AccountID  string    `json:"account_id"`
	SegmentID  string    `json:"segment_id"`
	UserID     string    `json:"user_id"`
	Tag        string    `json:"tag,omitempty"`
	EnrolledAt time.Time `json:"enrolled_at"`
}

// BehaviourSignalEvent is one consent-gated behavioural observation — a
// self-contained row for the behaviour_signals Delta table. Kind "request"
// rows come from the SSP (content categories stamped at event time from its
// placement warm cache); impression/click/conversion/view rows come from the
// tracker. Never published for non-consented users: the SSP checks
// privacy.Evaluate(...).Personalise, and the tracker only sees a user key
// when the consented serve path baked one into the pixel URL.
type BehaviourSignalEvent struct {
	SchemaVersion int    `json:"schema_version"`
	TraceID       string `json:"trace_id"`
	Kind          string `json:"kind"` // request | impression | click | conversion | view
	UserID        string `json:"user_id,omitempty"`
	HouseholdID   string `json:"household_id,omitempty"`
	PlacementID   string `json:"placement_id,omitempty"`
	PublisherID   string `json:"publisher_id,omitempty"`
	CampaignID    string `json:"campaign_id,omitempty"`
	CreativeID    string `json:"creative_id,omitempty"`
	Channel       string `json:"channel,omitempty"`
	Categories    string `json:"categories,omitempty"` // comma-separated content categories
	Geo           string `json:"geo,omitempty"`
	Device        string `json:"device,omitempty"`
	// AccountID + Tag carry retargeting-pixel attribution (kind
	// "site_visit"): the advertiser account whose site fired the pixel and
	// its self-chosen tag ("product-page"). Rules scope site_visit rows to
	// the segment's own account so tag names can't collide across tenants.
	AccountID  string    `json:"account_id,omitempty"`
	Tag        string    `json:"tag,omitempty"`
	ObservedAt time.Time `json:"observed_at"`
}

// ReportCompletedEvent announces a finished report job. Published by the
// report-job executor after the artifact is durable; the webhooks
// dispatcher delivers it to the account's subscriptions (event type
// "report.completed"). DownloadURL requires an authenticated session — the
// artifact bucket is private and only streams through the gateway.
type ReportCompletedEvent struct {
	SchemaVersion int       `json:"schema_version"`
	AccountID     string    `json:"account_id"`
	JobID         string    `json:"job_id"`
	Name          string    `json:"name"`
	Format        string    `json:"format"`
	RowCount      int64     `json:"row_count"`
	ArtifactBytes int64     `json:"artifact_bytes"`
	DownloadURL   string    `json:"download_url"`
	ExpiresAt     time.Time `json:"expires_at"`
	Timestamp     time.Time `json:"timestamp"`
}

// ProfileSignalID is one identifier inside a ProfileSignalEvent batch.
type ProfileSignalID struct {
	IDType  string `json:"id_type"` // user_id | hashed_email | uid2 | device_id | household
	IDValue string `json:"id_value"`
}

// ProfileSignalEvent is a BATCH of normalized audience-onboarding signals —
// one message per upload chunk, not per id, so a 50k-row CRM upload is ~50
// JetStream publishes rather than 50k. Published by the gateway on portal /
// API audience uploads; the pipeline's datalake sink expands each batch into
// one profile_signals lake row per id. The lake copy is what makes segment
// memberships recomputable (replay signals → rebuild memberships); the PG
// membership rows are written synchronously by the uploader.
type ProfileSignalEvent struct {
	SchemaVersion int `json:"schema_version"`
	// TraceID is the real request trace ONLY: the upload request's OTel trace when
	// the file is processed inline (synchronous), empty when the async worker
	// processes it (no request → no fake trace). Use IngestTraceID for lineage.
	TraceID string `json:"trace_id"`
	// IngestTraceID is the batch-lineage correlation for uploaded data, always set:
	// "ing_<32hex>" derived from the ingest job id (audience_ingest_jobs.id). Its
	// distinct ing_ prefix means it is never confused with a 32-hex request trace_id.
	IngestTraceID string            `json:"ingest_trace_id,omitempty"`
	AccountID     string            `json:"account_id"`
	Provider      string            `json:"provider,omitempty"`    // drop-zone provider name; empty = first-party
	ProviderID    string            `json:"provider_id,omitempty"` // data_providers.id (ADR 0009); empty = no provider
	DataParty     string            `json:"data_party,omitempty"`  // first | second | third (ADR 0009); empty = first
	Source        string            `json:"source"`                // crm_upload | portal_csv | dropzone
	Access        string            `json:"access"`                // first_party | purchased:{provider} | barter:{provider}
	SegmentID     string            `json:"segment_id"`
	SegmentName   string            `json:"segment_name"`
	Visibility    string            `json:"visibility"` // public | dsp_private
	Consent       bool              `json:"consent"`    // declared consent basis permits personalisation
	ObservedAt    time.Time         `json:"observed_at"`
	IDs           []ProfileSignalID `json:"ids"`
}

// AudienceInvalidateEvent is the payload of adtech.cache.invalidate.audience. It
// tells the DSP/SSP audience preloader WHAT changed so it can re-materialize only
// the affected Redis keys instead of rescanning the whole membership table:
//   - UserIDs set     → re-materialize exactly those users (real-time retargeting
//     enroll/suppress — the high-frequency path; removals MUST take this route,
//     since a segment-scoped refresh can't see a user already gone from a segment).
//   - else SegmentID  → re-materialize the users currently in that segment (uploads,
//     profile-builder segment rebuilds — bounded by segment, avoids stuffing a bulk
//     upload's ids into one message).
//   - else (empty)    → full reconcile (unknown/legacy source).
//
// The periodic full reconcile in the preloader is the backstop for anything a delta
// misses (silent TTL expiry, batch prunes) — deltas give speed, the reconcile gives
// eventual correctness. All fields optional; an empty/garbled payload → full refresh.
type AudienceInvalidateEvent struct {
	SchemaVersion int      `json:"schema_version"`
	Source        string   `json:"source,omitempty"`
	AccountID     string   `json:"account_id,omitempty"`
	SegmentID     string   `json:"segment_id,omitempty"`
	UserIDs       []string `json:"user_ids,omitempty"`
}

// PrebidOutboundWinEvent is published by cmd/publisher-adserver when an
// external Prebid Server's bid wins the programmatic comparison against
// our SSP. We don't bill these (the money flows outside our system), but
// we record them so reporting can show "publisher X earned $Y from
// external Prebid demand source Z" and so the trace explorer shows the
// served impression rather than appearing as nobid.
type PrebidOutboundWinEvent struct {
	SchemaVersion  int       `json:"schema_version"`
	TraceID        string    `json:"trace_id"`
	PublisherID    string    `json:"publisher_id"`
	PlacementID    string    `json:"placement_id"`
	PrebidEndpoint string    `json:"prebid_endpoint"` // which external server bid
	Seat           string    `json:"seat"`            // their identifier for the buyer
	ClearingPrice  float64   `json:"clearing_price"`
	Currency       string    `json:"currency"`
	DealID         string    `json:"deal_id,omitempty"`
	Timestamp      time.Time `json:"timestamp"`
}
