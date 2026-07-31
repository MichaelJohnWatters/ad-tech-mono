// Package analytics provides the pluggable analytics store interface.
//
// The analytics store handles append-only event data for reporting and
// aggregation. Two implementations exist behind the same interface:
//
//   - DuckDB (embedded, zero infra, ideal for local dev and staging)
//   - ClickHouse (server-based, scales to billions, ideal for prod)
//
// The reporting service writes events here via NATS consumers.
// Queries are used by the report builder and dashboard APIs.
//
// Usage:
//
//	store, err := analytics.NewDuckDB("/tmp/analytics.db")
//	store.InsertImpression(ctx, &analytics.ImpressionEvent{...})
//	results, err := store.Query(ctx, analytics.QueryParams{...})
package analytics

import (
	"context"
	"time"
)

// Store is the analytics data store interface.
// Implementations: DuckDB (duckdb.go), Memory (memory.go), ClickHouse (future).
type Store interface {
	// Write path
	InsertImpression(ctx context.Context, e *ImpressionEvent) error
	InsertClick(ctx context.Context, e *ClickEvent) error
	InsertConversion(ctx context.Context, e *ConversionEvent) error
	InsertView(ctx context.Context, e *ViewEvent) error
	InsertAuction(ctx context.Context, e *AuctionEvent) error
	InsertAuctionWin(ctx context.Context, e *AuctionWinEvent) error
	// InsertMediaEvent stores one video / audio engagement record
	// (start / firstQuartile / midpoint / thirdQuartile / complete /
	// mute / pause / resume / skip / fullscreen, plus the analogous
	// audio events). Used by the reporting service when consuming
	// adtech.events.video / .audio off NATS. Channel is "video" or
	// "audio"; the row stores both so a single media_events table
	// can serve both consumers.
	InsertMediaEvent(ctx context.Context, e *MediaEvent) error
	InsertBatch(ctx context.Context, events []Event) error

	// Read path
	Query(ctx context.Context, params QueryParams) (*QueryResult, error)

	// Lifecycle
	Close() error
}

// ObservabilityWriter is the set of operational-signal writes the
// reporting consumer makes alongside the core Store events. These are
// non-bid state transitions — serve suppressions, render failures, fraud
// rejections, budget depletions, campaign state changes, serve no-fills —
// surfaced to ops dashboards and advertiser reports rather than billed.
//
// Kept off the core Store interface (they're a reporting-side concern, not
// every analytics consumer's), but both MemoryStore and DuckDB implement
// it so reporting persists these on whichever backend is selected instead
// of dropping them on anything but memory. Signatures intentionally take
// no ctx/error to match the fire-and-forget call sites; implementations
// log their own failures.
type ObservabilityWriter interface {
	InsertServeNoFill(ServeNoFill)
	InsertFreqCapBlock(FreqCapBlock)
	InsertRenderFailure(RenderFailure)
	InsertTrackerRejection(TrackerRejection)
	InsertCampaignStateChange(CampaignStateChange)
	InsertBudgetDepletion(BudgetDepletion)
}

var (
	_ ObservabilityWriter = (*MemoryStore)(nil)
	_ ObservabilityWriter = (*ClickHouse)(nil)
	_ ObservabilityWriter = (*HotColdStore)(nil)
)

// CommittedReader recomputes per-campaign committed spend (in micro-dollars) for
// a UTC day directly from the raw event stream. It is the authoritative,
// replica-count-INDEPENDENT source the reporting service uses to periodically
// reconcile its shared pacing counter (see cmd/reporting's committed counter):
// because it sums every impression regardless of which replica ingested it, the
// total is correct no matter how the NATS consumer split the stream.
//
// Optional capability — ClickHouse implements it; memory/DuckDB backends may not,
// in which case the shared counter runs additive-only (no store self-heal).
type CommittedReader interface {
	// CommittedByCampaign returns campaign_id -> committed micro-dollars for the
	// given UTC day ("2006-01-02").
	CommittedByCampaign(ctx context.Context, day string) (map[string]int64, error)
}

// PublisherImpressionReader is the optional capability behind tiered
// revenue-share: publisher_id -> impression count since `since` (month start),
// so a publisher crossing a volume tier mid-month gets the better fee split.
// ClickHouse + MemoryStore implement it.
type PublisherImpressionReader interface {
	ImpressionsByPublisher(ctx context.Context, since time.Time) (map[string]int64, error)
}

// ViewableImpression is one prior ad exposure a view-through conversion can be
// credited to: the impression's trace, its campaign, when it was served, and
// whether it was IAB-viewable. Sourced from behaviour_signals (which carries the
// consented user_id impressions lack) joined to the views table for the verdict.
type ViewableImpression struct {
	TraceID    string
	CampaignID string
	Timestamp  time.Time
	Viewable   bool
}

// ViewThroughReader is the optional capability behind view-through attribution:
// the most-recent ad exposures for a set of platform user ids, scoped to an
// advertiser account (and optionally one campaign), since a lookback point.
// requireViewable filters to IAB-viewable exposures in the store. Results are
// ordered most-recent-first so the caller can take the last touch. ClickHouse +
// MemoryStore implement it; discovered by type assertion.
type ViewThroughReader interface {
	ViewableImpressionsForUsers(ctx context.Context, userIDs []string, accountID, campaignID string, since time.Time, requireViewable bool) ([]ViewableImpression, error)
}

var (
	_ ViewThroughReader = (*ClickHouse)(nil)
	_ ViewThroughReader = (*MemoryStore)(nil)
	_ ViewThroughReader = (*HotColdStore)(nil)
)

// AttributionTouchpointRow is one exposure in a conversion's multi-touch chain.
// The chain is stored model-agnostically (just the touchpoints + timings);
// fractional credit per model is computed on read via pkg/attribution, so a new
// model needs no re-write. Billing still settles last-touch — these rows are the
// reporting picture only.
type AttributionTouchpointRow struct {
	ConversionTraceID string
	TouchpointTraceID string
	AccountID         string
	CampaignID        string
	TouchpointType    string // impression | click | view
	TouchpointAt      time.Time
	ConversionAt      time.Time
	ConversionRevenue float64
	ObservedAt        time.Time
}

// AttributionWriter persists multi-touch attribution chains. Optional capability
// (ClickHouse + MemoryStore); reporting no-ops when the store lacks it.
type AttributionWriter interface {
	InsertAttributionTouchpoints(ctx context.Context, rows []*AttributionTouchpointRow) error
}

var (
	_ AttributionWriter = (*ClickHouse)(nil)
	_ AttributionWriter = (*MemoryStore)(nil)
	_ AttributionWriter = (*HotColdStore)(nil)
)

// RollupRow is one aggregated row produced by the rollup engine: a set of
// dimension values + metric values for a (config, level, time-window).
// Dimensions and metrics are maps rather than fixed columns so a single
// rollups table serves every rollup config (events, auctions, …) — the
// "universal rollup framework". Persisted as JSON columns.
type RollupRow struct {
	Config     string             `json:"config"`
	Level      string             `json:"level"`
	WindowFrom time.Time          `json:"window_from"`
	WindowTo   time.Time          `json:"window_to"`
	Dimensions map[string]string  `json:"dimensions"`
	Metrics    map[string]float64 `json:"metrics"`
}

// RollupWriter persists aggregated rollup rows. Both MemoryStore and
// DuckDB implement it so the rollup engine writes through whichever
// analytics backend is selected. Kept off the core Store interface for the
// same reason as ObservabilityWriter — it's a reporting/rollup concern.
type RollupWriter interface {
	InsertRollups(ctx context.Context, rows []RollupRow) error
}

var _ RollupWriter = (*MemoryStore)(nil)

// RollupReader reads back persisted rollup rows for a (config, level) whose
// window overlaps [from, to]. It's the read side of the rollup framework: the
// reporting builder's tiered-read path (AutoTier) selects a tier via
// TierForRange and re-aggregates these rows instead of scanning raw events.
// Implemented by MemoryStore, ClickHouse and DuckDB (all persist rollups).
type RollupReader interface {
	QueryRollups(ctx context.Context, config, level string, from, to time.Time) ([]RollupRow, error)
}

var (
	_ RollupReader = (*MemoryStore)(nil)
	_ RollupReader = (*ClickHouse)(nil)
)

// BatchInserter is the bulk write path — one call inserts many rows of a
// single event type as one atomic block. It's the ingest primitive behind
// the reporting service's NATS batch consumer: a JetStream fetch of N
// messages becomes one INSERT per table, instead of N single-row inserts
// (the ClickHouse "too many parts" anti-pattern).
//
// Kept off the core Store interface (same convention as ObservabilityWriter
// and RollupWriter — it's a reporting-side ingest concern, discovered by
// type assertion). ClickHouse implements it with clickhouse-go's native
// PrepareBatch; MemoryStore and DuckDB implement it by looping their
// existing single-row inserts, so callers get parity on every backend.
//
// Each method is a no-op on an empty slice and is all-or-nothing: on the
// ClickHouse backend a failed Send() commits zero rows, which is what lets
// the batch consumer ack-all-or-nak-all without partial writes.
type BatchInserter interface {
	InsertImpressions(ctx context.Context, es []*ImpressionEvent) error
	InsertClicks(ctx context.Context, es []*ClickEvent) error
	InsertConversions(ctx context.Context, es []*ConversionEvent) error
	InsertViews(ctx context.Context, es []*ViewEvent) error
	InsertAuctions(ctx context.Context, es []*AuctionEvent) error
	InsertAuctionWins(ctx context.Context, es []*AuctionWinEvent) error
	InsertMediaEvents(ctx context.Context, es []*MediaEvent) error
	InsertDSPCalls(ctx context.Context, es []*DSPCallEvent) error
	// Profile-store tables (ADR 0006 phase 1): land the consent-gated
	// behavioural observations and the expanded onboarding-signal id rows in
	// ClickHouse alongside the existing lake dual-write, so phase 2 can repoint
	// the profile-builder off its OOM-prone Arrow lake read onto server-side
	// GROUP BY. ProfileSignalRow is the PER-ID expansion of one
	// events.ProfileSignalEvent batch (mirrors the pipeline lake sink), so both
	// stores hold the same one-row-per-id shape.
	InsertBehaviourSignals(ctx context.Context, es []*BehaviourSignalRow) error
	InsertProfileSignals(ctx context.Context, es []*ProfileSignalRow) error
}

var _ BatchInserter = (*MemoryStore)(nil)

// DebugReader is the set of read-back queries the reporting service's /debug
// endpoints use (consumed by the e2e harness and ops tooling to assert an
// event reached the store). Kept off the core Store interface — it's a debug
// affordance. MemoryStore has always implemented these; ClickHouse implements
// them too so the full-local stack (clickhouse backend) keeps the debug
// endpoints working instead of degrading to 501.
type DebugReader interface {
	AuctionWinCount(traceID string) int
	AuctionWinByBidModel(traceID, bidModel string) int
	BudgetDepletionsByCampaign(campaignID string) int
	CampaignStateChangesByCampaign(campaignID string) []CampaignStateChange
	RenderFailuresByCreative(creativeID string) []RenderFailure
	FreqCapBlocksByCampaign(campaignID string) []FreqCapBlock
	TrackerRejectionsByTrace(traceID, reason string) []TrackerRejection
	TrackerRejectionsByReason(reason string) int
	ServeNoFillsByTrace(traceID string) int
	MediaEventsByTrace(traceID, channel, eventType string) int
}

var (
	_ DebugReader = (*MemoryStore)(nil)
	_ DebugReader = (*ClickHouse)(nil)
)

// Event is a tagged union for batch inserts.
type Event struct {
	Type       EventType        `json:"type"`
	Impression *ImpressionEvent `json:"impression,omitempty"`
	Click      *ClickEvent      `json:"click,omitempty"`
	Conversion *ConversionEvent `json:"conversion,omitempty"`
	View       *ViewEvent       `json:"view,omitempty"`
	Auction    *AuctionEvent    `json:"auction,omitempty"`
}

// EventType identifies the kind of event.
type EventType string

const (
	EventImpression EventType = "impression"
	EventClick      EventType = "click"
	EventConversion EventType = "conversion"
	EventView       EventType = "view"
	EventAuction    EventType = "auction"
)

// ImpressionEvent records a served impression.
type ImpressionEvent struct {
	SchemaVersion    int       `json:"schema_version"`
	TraceID          string    `json:"trace_id"`
	InsertionOrderID string    `json:"insertion_order_id,omitempty"`
	CampaignID       string    `json:"campaign_id"`
	CreativeID       string    `json:"creative_id"`
	PlacementID      string    `json:"placement_id"`
	PublisherID      string    `json:"publisher_id"`
	AccountID        string    `json:"account_id"`
	Geo              string    `json:"geo,omitempty"`
	Device           string    `json:"device,omitempty"`
	Channel          string    `json:"channel,omitempty"`
	Format           string    `json:"format,omitempty"`
	ClearingPrice    float64   `json:"clearing_price"`
	ClearingCurrency string    `json:"clearing_currency"`
	ClearingPriceUSD float64   `json:"clearing_price_usd"`
	BidModel         string    `json:"bid_model,omitempty"`
	DealID           string    `json:"deal_id,omitempty"`
	Timestamp        time.Time `json:"timestamp"`
}

// ClickEvent records a click on a served ad.
type ClickEvent struct {
	SchemaVersion int       `json:"schema_version"`
	TraceID       string    `json:"trace_id"`
	CampaignID    string    `json:"campaign_id"`
	CreativeID    string    `json:"creative_id"`
	PlacementID   string    `json:"placement_id"`
	PublisherID   string    `json:"publisher_id"`
	AccountID     string    `json:"account_id"`
	LandingURL    string    `json:"landing_url,omitempty"`
	Geo           string    `json:"geo,omitempty"`
	Device        string    `json:"device,omitempty"`
	Timestamp     time.Time `json:"timestamp"`
}

// ViewEvent records a viewability beacon for a served impression. Distinct
// from ImpressionEvent because viewability is measured after the ad has
// rendered (the client observes intersection + dwell time). One impression
// can have zero or one view event — zero means the ad never met the
// viewability bar, one means it did and the client beaconed back.
//
// IABViewable is computed server-side from DurationMs + PercentVisible
// (and AreaPx when the client passes it) so the analytics row stores the
// authoritative verdict, not the client's claim.
type ViewEvent struct {
	SchemaVersion  int    `json:"schema_version"`
	TraceID        string `json:"trace_id"`
	CampaignID     string `json:"campaign_id"`
	CreativeID     string `json:"creative_id,omitempty"`
	PlacementID    string `json:"placement_id"`
	PublisherID    string `json:"publisher_id"`
	AccountID      string `json:"account_id"`
	// Channel is "display" (or empty) or "video" — the IAB viewability dwell
	// differs (1s vs 2s), and it lets reporting split display vs video
	// viewability instead of conflating them.
	Channel        string    `json:"channel,omitempty"`
	DurationMs     int64     `json:"duration_ms"`
	PercentVisible int       `json:"percent_visible"`
	AreaPx         int64     `json:"area_px,omitempty"`
	IABViewable    bool      `json:"iab_viewable"`
	Timestamp      time.Time `json:"timestamp"`
}

// IsIABViewable applies the IAB/MRC viewability rule for the given channel: at
// least 50% of pixels on-screen for at least the minimum continuous dwell —
// **1 second for display, 2 seconds for VIDEO** (the standards differ). Large
// display ads (>= 242,500 px²) drop to a 30% threshold per the IAB Large Format
// Standard. AreaPx == 0 means "client didn't tell us the area" → default 50%.
// An empty channel is treated as display (the historical default).
func IsIABViewable(durationMs int64, percentVisible int, areaPx int64, channel string) bool {
	minDwellMs := int64(1000) // display: >= 1s
	if channel == "video" {
		minDwellMs = 2000 // IAB/MRC video: >= 2s continuous
	}
	if durationMs < minDwellMs {
		return false
	}
	threshold := 50
	if channel != "video" && areaPx >= 242500 {
		threshold = 30 // large-format display only
	}
	return percentVisible >= threshold
}

// ConversionEvent records a conversion (purchase, signup, etc.).
type ConversionEvent struct {
	SchemaVersion  int       `json:"schema_version"`
	TraceID        string    `json:"trace_id"`
	CampaignID     string    `json:"campaign_id"`
	CreativeID     string    `json:"creative_id"`
	PlacementID    string    `json:"placement_id"`
	AccountID      string    `json:"account_id"`
	ConversionType string    `json:"conversion_type"`
	Revenue        float64   `json:"revenue,omitempty"`
	Currency       string    `json:"currency,omitempty"`
	RevenueUSD     float64   `json:"revenue_usd,omitempty"`
	// AttributedTraceID is the trace of the ad exposure (impression/click) this
	// conversion is credited to — the join that closes the attribution loop.
	// For deterministic click-through it is the earning click's trace, captured
	// on the landing page and returned by the advertiser (the gclid analog).
	// Empty = unattributed (billing falls back to the conversion's own trace).
	AttributedTraceID string `json:"attributed_trace_id,omitempty"`
	// AttributionType is how the credit was assigned: "click_through" |
	// "view_through" | "" (unattributed). Phase 0 only sets "click_through".
	AttributionType string `json:"attribution_type,omitempty"`
	// UserID is the advertiser-side first-party visitor id carried on the
	// conversion (used by later phases to resolve view-through/cross-device).
	UserID    string    `json:"user_id,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// SettleTraceID is the trace the CPA reservation lives on: the attributed
// exposure trace (impression/click) when attribution resolved it, else the
// conversion's own trace (legacy / unattributed — normally reservation-less).
// Both the single and batch conversion consumers settle against this.
func (e *ConversionEvent) SettleTraceID() string {
	if e.AttributedTraceID != "" {
		return e.AttributedTraceID
	}
	return e.TraceID
}

// AuctionEvent records an auction outcome.
type AuctionEvent struct {
	SchemaVersion    int       `json:"schema_version"`
	TraceID          string    `json:"trace_id"`
	PlacementID      string    `json:"placement_id"`
	PublisherID      string    `json:"publisher_id"`
	Channel          string    `json:"channel,omitempty"`
	NumBids          int       `json:"num_bids"`
	WinningBid       float64   `json:"winning_bid,omitempty"`
	ClearingPrice    float64   `json:"clearing_price,omitempty"`
	Currency         string    `json:"currency,omitempty"`
	ClearingPriceUSD float64   `json:"clearing_price_usd,omitempty"`
	WinnerDSP        string    `json:"winner_dsp,omitempty"`
	DurationMs       int64     `json:"duration_ms"`
	DealID           string    `json:"deal_id,omitempty"`
	Timestamp        time.Time `json:"timestamp"`
}

// AuctionWinEvent records the winning bid of an auction — the moment a buyer
// commits to a clearing price. Distinct from AuctionEvent (full auction
// snapshot with all bids) because it's the financial trigger: every win is a
// candidate spend that should bill once delivery is confirmed.
//
// Wire-format mirror of pkg/events.AuctionWinEvent. Kept here so the
// analytics layer doesn't need to import pkg/events.
type AuctionWinEvent struct {
	SchemaVersion int       `json:"schema_version"`
	TraceID       string    `json:"trace_id"`
	AuctionID     string    `json:"auction_id,omitempty"`
	WinnerDSP     string    `json:"winner_dsp"`
	CampaignID    string    `json:"campaign_id"`
	CreativeID    string    `json:"creative_id,omitempty"`
	PlacementID   string    `json:"placement_id"`
	PublisherID   string    `json:"publisher_id"`
	AdvertiserID  string    `json:"advertiser_id"`
	ClearingPrice float64   `json:"clearing_price"`
	Currency      string    `json:"currency,omitempty"`
	BidModel      string    `json:"bid_model,omitempty"`
	DealID        string    `json:"deal_id,omitempty"`
	Channel       string    `json:"channel,omitempty"`
	Timestamp     time.Time `json:"timestamp"`
}

// DSPCallStat is a per-(channel, DSP endpoint) aggregate of dsp_calls over a
// window — the shape the exchange warm-starts its SmartRouter from on boot.
type DSPCallStat struct {
	Channel       string  `json:"channel"`
	DSPEndpoint   string  `json:"dsp_endpoint"`
	TotalCalls    int64   `json:"total_calls"`
	TotalBids     int64   `json:"total_bids"`
	TotalTimeouts int64   `json:"total_timeouts"`
	AvgBidUSD     float64 `json:"avg_bid_usd"`
	AvgLatencyMs  float64 `json:"avg_latency_ms"`
}

// DSPCallAggregator returns per-(channel, endpoint) dsp_calls aggregates since
// a cutoff. Implemented by ClickHouse (GROUP BY) and MemoryStore (in-process);
// consumed by the reporting routing-stats endpoint that the exchange
// warm-starts from. Optional capability, discovered by type assertion.
type DSPCallAggregator interface {
	DSPCallStats(ctx context.Context, since time.Time) ([]DSPCallStat, error)
}

var (
	_ DSPCallAggregator = (*MemoryStore)(nil)
	_ DSPCallAggregator = (*ClickHouse)(nil)
)

// CreativeStat is a per-creative impressions+clicks aggregate over a window —
// the shape the ad server warm-starts its Thompson-sampling bandit from on boot
// (ADR 0003 part C). No new event stream: derived from the impressions and
// clicks already in the store.
type CreativeStat struct {
	CreativeID  string `json:"creative_id"`
	Impressions int64  `json:"impressions"`
	Clicks      int64  `json:"clicks"`
}

// CreativeStatAggregator returns per-creative impressions/clicks since a cutoff.
// Implemented by ClickHouse and MemoryStore; consumed by the reporting
// creative-stats endpoint the ad server warm-starts from. Optional capability.
type CreativeStatAggregator interface {
	CreativeStats(ctx context.Context, since time.Time) ([]CreativeStat, error)
}

var (
	_ CreativeStatAggregator = (*MemoryStore)(nil)
	_ CreativeStatAggregator = (*ClickHouse)(nil)
)

// TraceScope restricts a trace/impression read to one tenant. Exactly one of
// AccountID (advertiser — the impression's account_id column) or PublisherID
// (publisher — the impression's publisher_id column) is set for a scoped caller;
// both empty means staff/unscoped. The gateway is responsible for validating the
// value against the session before it reaches here (same as the report path).
type TraceScope struct {
	AccountID   string
	PublisherID string
}

// Unscoped reports whether the scope imposes no tenant restriction (staff).
func (s TraceScope) Unscoped() bool { return s.AccountID == "" && s.PublisherID == "" }

// TraceEvent is one recorded event on a single trace, normalised across the
// per-kind analytics tables so the trace inspector can build one timeline.
// Financial/identity fields are ALL present here; the reporting handler REDACTS
// them per audience before returning (advertiser sees no margin, publisher sees
// no advertiser identity, etc.) — never redact in the store.
type TraceEvent struct {
	Kind             string    `json:"kind"` // impression|click|conversion|view|auction_win|media
	Timestamp        time.Time `json:"timestamp"`
	CampaignID       string    `json:"campaign_id,omitempty"`
	CreativeID       string    `json:"creative_id,omitempty"`
	PlacementID      string    `json:"placement_id,omitempty"`
	PublisherID      string    `json:"publisher_id,omitempty"`
	AdvertiserID     string    `json:"advertiser_id,omitempty"`
	AccountID        string    `json:"account_id,omitempty"`
	WinnerDSP        string    `json:"winner_dsp,omitempty"`
	BidModel         string    `json:"bid_model,omitempty"`
	ClearingPriceUSD float64   `json:"clearing_price_usd,omitempty"`
	DealID           string    `json:"deal_id,omitempty"`
	EventType        string    `json:"event_type,omitempty"` // media quartile / view verdict / conversion type
	// DSPEndpoint + NoBidReason are set on "dsp_block" events (Phase H): a DSP
	// declined a bid for a stated reason (e.g. adcert_invalid), surfaced in the
	// trace so the block isn't lost in the aggregated no-bid.
	DSPEndpoint string `json:"dsp_endpoint,omitempty"`
	NoBidReason string `json:"no_bid_reason,omitempty"`
}

// ImpressionRow is one recent impression for the portal "View trace" drill-down
// list. Scoped by TraceScope; newest first.
type ImpressionRow struct {
	TraceID          string    `json:"trace_id"`
	Timestamp        time.Time `json:"timestamp"`
	CampaignID       string    `json:"campaign_id"`
	CreativeID       string    `json:"creative_id"`
	PlacementID      string    `json:"placement_id"`
	PublisherID      string    `json:"publisher_id"`
	BidModel         string    `json:"bid_model,omitempty"`
	ClearingPriceUSD float64   `json:"clearing_price_usd"`
	DealID           string    `json:"deal_id,omitempty"`
}

// TraceReader reconstructs a single trace and lists recent impressions for the
// trace inspector. Optional capability (ClickHouse + MemoryStore implement it),
// discovered by type assertion like the other *Reader/*Aggregator interfaces.
type TraceReader interface {
	// EventsByTrace returns every recorded event for traceID, ordered by time.
	// If scope is set, the caller must "own" the trace — at least one row must
	// match the scope column — else it returns an empty slice (→ 404 upstream).
	// Once ownership is proven the whole trace is returned (a trace is one
	// request, so all its rows belong to that one impression's parties).
	EventsByTrace(ctx context.Context, traceID string, scope TraceScope) ([]TraceEvent, error)
	// RecentImpressions lists the most recent impressions for the scope, newest
	// first, capped at limit.
	RecentImpressions(ctx context.Context, scope TraceScope, limit int) ([]ImpressionRow, error)
}

var (
	_ TraceReader = (*MemoryStore)(nil)
	_ TraceReader = (*ClickHouse)(nil)
)

// DSPCallEvent is the analytics mirror of events.DSPCallEvent — one row per
// DSP fan-out call in an auction (routing telemetry). Win attribution is by
// join to auction_wins on trace_id, not a column here.
type DSPCallEvent struct {
	SchemaVersion int       `json:"schema_version"`
	TraceID       string    `json:"trace_id"`
	AuctionID     string    `json:"auction_id"`
	Channel       string    `json:"channel"`
	DSPEndpoint   string    `json:"dsp_endpoint"`
	BidReceived   bool      `json:"bid_received"`
	BidPriceUSD   float64   `json:"bid_price_usd"`
	LatencyMs     int64     `json:"latency_ms"`
	TimedOut      bool      `json:"timed_out"`
	NoBidReason   string    `json:"no_bid_reason,omitempty"` // DSP's stated no-bid reason (e.g. adcert_invalid); empty for a bid or plain no-demand
	Timestamp     time.Time `json:"timestamp"`
}

// BehaviourSignalRow is the analytics mirror of events.BehaviourSignalEvent —
// one consent-gated behavioural observation (a "request" row from the SSP, an
// impression/click/conversion/view row from the tracker, or a "site_visit" row
// from a retargeting pixel). Column shape matches the lake behaviour_signals
// table so phase 2's rule GROUP BY lines up across both stores. Never written
// for non-consented users (the publishers gate this upstream).
type BehaviourSignalRow struct {
	TraceID     string `json:"trace_id"`
	Kind        string `json:"kind"`
	UserID      string `json:"user_id,omitempty"`
	HouseholdID string `json:"household_id,omitempty"`
	PlacementID string `json:"placement_id,omitempty"`
	PublisherID string `json:"publisher_id,omitempty"`
	CampaignID  string `json:"campaign_id,omitempty"`
	CreativeID  string `json:"creative_id,omitempty"`
	Channel     string `json:"channel,omitempty"`
	Categories  string `json:"categories,omitempty"`
	Geo         string `json:"geo,omitempty"`
	Device      string `json:"device,omitempty"`
	// AccountID + Tag carry retargeting-pixel attribution (kind "site_visit"):
	// the advertiser account whose site fired the pixel and its self-chosen tag.
	AccountID  string    `json:"account_id,omitempty"`
	Tag        string    `json:"tag,omitempty"`
	ObservedAt time.Time `json:"observed_at"`
}

// ProfileSignalRow is the EXPANDED per-id shape of one id inside an
// events.ProfileSignalEvent batch: the event's shared onboarding fields plus
// the single (IDType, IDValue) this row is about. One event with N ids becomes
// N ProfileSignalRows — exactly the expansion the pipeline lake sink does
// (profileSignalRecord), so the ClickHouse profile_signals table holds the same
// one-row-per-id record and phase 2's membership rebuild queries match.
type ProfileSignalRow struct {
	TraceID     string    `json:"trace_id"`
	AccountID   string    `json:"account_id"`
	Provider    string    `json:"provider,omitempty"`
	ProviderID  string    `json:"provider_id,omitempty"` // data_providers.id (ADR 0009)
	DataParty   string    `json:"data_party,omitempty"`  // first | second | third (ADR 0009)
	Source      string    `json:"source"`
	Access      string    `json:"access"`
	SegmentID   string    `json:"segment_id"`
	SegmentName string    `json:"segment_name"`
	Visibility  string    `json:"visibility"`
	Consent     bool      `json:"consent"`
	IDType      string    `json:"id_type"`
	IDValue     string    `json:"id_value"`
	ObservedAt  time.Time `json:"observed_at"`
}

// QueryParams defines a query against the analytics store.
type QueryParams struct {
	Table      string            `json:"table"`
	Metrics    []string          `json:"metrics,omitempty"`
	Dimensions []string          `json:"dimensions,omitempty"`
	Filters    map[string]string `json:"filters,omitempty"`
	TimeFrom   time.Time         `json:"time_from,omitempty"`
	TimeTo     time.Time         `json:"time_to,omitempty"`
	Limit      int               `json:"limit,omitempty"`
	OrderBy    string            `json:"order_by,omitempty"`
	OrderDir   string            `json:"order_dir,omitempty"`
}

// QueryResult holds the output of an analytics query.
type QueryResult struct {
	Columns []string        `json:"columns"`
	Rows    [][]interface{} `json:"rows"`
	// Approximate is non-empty when the answer is knowingly PARTIAL —
	// e.g. the cold store failed and only the hot window was served. The
	// clean-slate cold boot (2026-07-19) proved why this must ride the
	// RESPONSE: a missing lake bucket degraded deep-history queries to
	// confidently wrong numbers while only a server-side WARN knew.
	Approximate string `json:"approximate,omitempty"`
}
