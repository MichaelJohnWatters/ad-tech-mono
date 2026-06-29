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

var _ ObservabilityWriter = (*MemoryStore)(nil)

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
	SchemaVersion  int       `json:"schema_version"`
	TraceID        string    `json:"trace_id"`
	CampaignID     string    `json:"campaign_id"`
	CreativeID     string    `json:"creative_id,omitempty"`
	PlacementID    string    `json:"placement_id"`
	PublisherID    string    `json:"publisher_id"`
	AccountID      string    `json:"account_id"`
	DurationMs     int64     `json:"duration_ms"`
	PercentVisible int       `json:"percent_visible"`
	AreaPx         int64     `json:"area_px,omitempty"`
	IABViewable    bool      `json:"iab_viewable"`
	Timestamp      time.Time `json:"timestamp"`
}

// IsIABViewable applies the IAB MRC display-ad rule: at least 50% pixels
// visible for at least 1 continuous second. Large ads (>= 242,500 px²)
// drop to a 30% threshold per the IAB Large Format Standard. AreaPx == 0
// means "client didn't tell us the area" and we use the default 50%.
func IsIABViewable(durationMs int64, percentVisible int, areaPx int64) bool {
	if durationMs < 1000 {
		return false
	}
	threshold := 50
	if areaPx >= 242500 {
		threshold = 30
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
	Timestamp      time.Time `json:"timestamp"`
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
}
