//go:build duckdb

package analytics

import (
	"context"
	"database/sql"
	"fmt"
	"sync"

	_ "github.com/marcboeker/go-duckdb"
)

// DuckDB is an embedded DuckDB analytics store.
// Zero infrastructure - just a file on disk.
// Used for local development and staging.
type DuckDB struct {
	db *sql.DB
	mu sync.Mutex // DuckDB writes are single-threaded
}

// NewDuckDB opens (or creates) a DuckDB database at the given path.
// Use ":memory:" for an in-memory database (testing).
func NewDuckDB(path string) (*DuckDB, error) {
	db, err := sql.Open("duckdb", path)
	if err != nil {
		return nil, fmt.Errorf("open duckdb %s: %w", path, err)
	}

	store := &DuckDB{db: db}
	if err := store.createTables(); err != nil {
		db.Close()
		return nil, fmt.Errorf("create tables: %w", err)
	}
	return store, nil
}

func (d *DuckDB) createTables() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS impressions (
			trace_id          VARCHAR NOT NULL,
			insertion_order_id VARCHAR,
			campaign_id       VARCHAR NOT NULL,
			creative_id       VARCHAR NOT NULL,
			placement_id      VARCHAR NOT NULL,
			publisher_id      VARCHAR NOT NULL,
			account_id        VARCHAR NOT NULL,
			geo               VARCHAR,
			device            VARCHAR,
			channel           VARCHAR,
			format            VARCHAR,
			clearing_price    DOUBLE NOT NULL,
			clearing_currency VARCHAR NOT NULL,
			clearing_price_usd DOUBLE NOT NULL,
			bid_model         VARCHAR,
			deal_id           VARCHAR,
			schema_version    INTEGER DEFAULT 1,
			timestamp         TIMESTAMP NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS clicks (
			trace_id      VARCHAR NOT NULL,
			campaign_id   VARCHAR NOT NULL,
			creative_id   VARCHAR NOT NULL,
			placement_id  VARCHAR NOT NULL,
			publisher_id  VARCHAR NOT NULL,
			account_id    VARCHAR NOT NULL,
			landing_url   VARCHAR,
			geo           VARCHAR,
			device        VARCHAR,
			schema_version INTEGER DEFAULT 1,
			timestamp     TIMESTAMP NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS conversions (
			trace_id        VARCHAR NOT NULL,
			campaign_id     VARCHAR NOT NULL,
			creative_id     VARCHAR NOT NULL,
			placement_id    VARCHAR NOT NULL,
			account_id      VARCHAR NOT NULL,
			conversion_type VARCHAR NOT NULL,
			revenue         DOUBLE,
			currency        VARCHAR,
			revenue_usd     DOUBLE,
			schema_version  INTEGER DEFAULT 1,
			timestamp       TIMESTAMP NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS auctions (
			trace_id         VARCHAR NOT NULL,
			placement_id     VARCHAR NOT NULL,
			publisher_id     VARCHAR NOT NULL,
			channel          VARCHAR,
			num_bids         INTEGER NOT NULL,
			winning_bid      DOUBLE,
			clearing_price   DOUBLE,
			currency         VARCHAR,
			clearing_price_usd DOUBLE,
			winner_dsp       VARCHAR,
			duration_ms      BIGINT NOT NULL,
			deal_id          VARCHAR,
			schema_version   INTEGER DEFAULT 1,
			timestamp        TIMESTAMP NOT NULL
		)`,
	}

	for _, stmt := range statements {
		if _, err := d.db.Exec(stmt); err != nil {
			return fmt.Errorf("exec %s: %w", stmt[:40], err)
		}
	}
	return nil
}

func (d *DuckDB) InsertImpression(ctx context.Context, e *ImpressionEvent) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	_, err := d.db.ExecContext(ctx,
		`INSERT INTO impressions (trace_id, insertion_order_id, campaign_id, creative_id,
			placement_id, publisher_id, account_id, geo, device, channel, format,
			clearing_price, clearing_currency, clearing_price_usd, bid_model, deal_id,
			schema_version, timestamp)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.TraceID, e.InsertionOrderID, e.CampaignID, e.CreativeID,
		e.PlacementID, e.PublisherID, e.AccountID, e.Geo, e.Device, e.Channel, e.Format,
		e.ClearingPrice, e.ClearingCurrency, e.ClearingPriceUSD, e.BidModel, e.DealID,
		e.SchemaVersion, e.Timestamp)
	return err
}

func (d *DuckDB) InsertClick(ctx context.Context, e *ClickEvent) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	_, err := d.db.ExecContext(ctx,
		`INSERT INTO clicks (trace_id, campaign_id, creative_id, placement_id, publisher_id,
			account_id, landing_url, geo, device, schema_version, timestamp)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.TraceID, e.CampaignID, e.CreativeID, e.PlacementID, e.PublisherID,
		e.AccountID, e.LandingURL, e.Geo, e.Device, e.SchemaVersion, e.Timestamp)
	return err
}

func (d *DuckDB) InsertConversion(ctx context.Context, e *ConversionEvent) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	_, err := d.db.ExecContext(ctx,
		`INSERT INTO conversions (trace_id, campaign_id, creative_id, placement_id,
			account_id, conversion_type, revenue, currency, revenue_usd, schema_version, timestamp)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.TraceID, e.CampaignID, e.CreativeID, e.PlacementID,
		e.AccountID, e.ConversionType, e.Revenue, e.Currency, e.RevenueUSD,
		e.SchemaVersion, e.Timestamp)
	return err
}

func (d *DuckDB) InsertAuction(ctx context.Context, e *AuctionEvent) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	_, err := d.db.ExecContext(ctx,
		`INSERT INTO auctions (trace_id, placement_id, publisher_id, channel, num_bids,
			winning_bid, clearing_price, currency, clearing_price_usd, winner_dsp,
			duration_ms, deal_id, schema_version, timestamp)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.TraceID, e.PlacementID, e.PublisherID, e.Channel, e.NumBids,
		e.WinningBid, e.ClearingPrice, e.Currency, e.ClearingPriceUSD, e.WinnerDSP,
		e.DurationMs, e.DealID, e.SchemaVersion, e.Timestamp)
	return err
}

// InsertAuctionWin records a winning bid. Schema TODO: needs an
// `auction_wins` table next to `auctions`. For now this stub keeps the
// interface satisfied so the in-memory store path works in dev; wire the
// real INSERT when DuckDB becomes the default analytics backend.
func (d *DuckDB) InsertAuctionWin(_ context.Context, _ *AuctionWinEvent) error {
	return nil
}

func (d *DuckDB) InsertBatch(ctx context.Context, events []Event) error {
	for _, e := range events {
		var err error
		switch e.Type {
		case EventImpression:
			err = d.InsertImpression(ctx, e.Impression)
		case EventClick:
			err = d.InsertClick(ctx, e.Click)
		case EventConversion:
			err = d.InsertConversion(ctx, e.Conversion)
		case EventAuction:
			err = d.InsertAuction(ctx, e.Auction)
		default:
			return fmt.Errorf("unknown event type: %s", e.Type)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (d *DuckDB) Query(ctx context.Context, params QueryParams) (*QueryResult, error) {
	query, args := BuildQuery(params)

	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("columns: %w", err)
	}

	var result QueryResult
	result.Columns = columns

	for rows.Next() {
		vals := make([]interface{}, len(columns))
		ptrs := make([]interface{}, len(columns))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		result.Rows = append(result.Rows, vals)
	}
	return &result, rows.Err()
}

func (d *DuckDB) Close() error {
	return d.db.Close()
}
