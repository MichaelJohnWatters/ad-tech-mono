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
	InsertAuction(ctx context.Context, e *AuctionEvent) error
	InsertBatch(ctx context.Context, events []Event) error

	// Read path
	Query(ctx context.Context, params QueryParams) (*QueryResult, error)

	// Lifecycle
	Close() error
}

// Event is a tagged union for batch inserts.
type Event struct {
	Type       EventType        `json:"type"`
	Impression *ImpressionEvent `json:"impression,omitempty"`
	Click      *ClickEvent      `json:"click,omitempty"`
	Conversion *ConversionEvent `json:"conversion,omitempty"`
	Auction    *AuctionEvent    `json:"auction,omitempty"`
}

// EventType identifies the kind of event.
type EventType string

const (
	EventImpression EventType = "impression"
	EventClick      EventType = "click"
	EventConversion EventType = "conversion"
	EventAuction    EventType = "auction"
)

// ImpressionEvent records a served impression.
type ImpressionEvent struct {
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
	SchemaVersion    int       `json:"schema_version"`
	Timestamp        time.Time `json:"timestamp"`
}

// ClickEvent records a click on a served ad.
type ClickEvent struct {
	TraceID       string    `json:"trace_id"`
	CampaignID    string    `json:"campaign_id"`
	CreativeID    string    `json:"creative_id"`
	PlacementID   string    `json:"placement_id"`
	PublisherID   string    `json:"publisher_id"`
	AccountID     string    `json:"account_id"`
	LandingURL    string    `json:"landing_url,omitempty"`
	Geo           string    `json:"geo,omitempty"`
	Device        string    `json:"device,omitempty"`
	SchemaVersion int       `json:"schema_version"`
	Timestamp     time.Time `json:"timestamp"`
}

// ConversionEvent records a conversion (purchase, signup, etc.).
type ConversionEvent struct {
	TraceID        string    `json:"trace_id"`
	CampaignID     string    `json:"campaign_id"`
	CreativeID     string    `json:"creative_id"`
	PlacementID    string    `json:"placement_id"`
	AccountID      string    `json:"account_id"`
	ConversionType string    `json:"conversion_type"`
	Revenue        float64   `json:"revenue,omitempty"`
	Currency       string    `json:"currency,omitempty"`
	RevenueUSD     float64   `json:"revenue_usd,omitempty"`
	SchemaVersion  int       `json:"schema_version"`
	Timestamp      time.Time `json:"timestamp"`
}

// AuctionEvent records an auction outcome.
type AuctionEvent struct {
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
	SchemaVersion    int       `json:"schema_version"`
	Timestamp        time.Time `json:"timestamp"`
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
