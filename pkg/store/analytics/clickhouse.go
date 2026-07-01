package analytics

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// ClickHouse is the server-based analytics store — the prod / full-local
// event backend (real-time ingest + fast aggregation). Unlike DuckDB its
// driver is pure Go (native protocol), so reporting stays CGO-free and runs
// as a normal pod. See docs/adr/0001-analytics-engines.md.
//
// Implements Store (+ ObservabilityWriter + RollupWriter) for full parity
// with the memory and DuckDB backends. All tables are MergeTree ordered by
// timestamp — the dominant filter/group dimension for reporting.
type ClickHouse struct {
	db  *sql.DB
	log *slog.Logger
}

// ClickHouseConfig configures the connection. Addrs is host:port pairs.
type ClickHouseConfig struct {
	Addrs    []string
	Database string
	Username string
	Password string
	Log      *slog.Logger
}

// NewClickHouse opens a connection, pings it, and ensures the schema.
func NewClickHouse(cfg ClickHouseConfig) (*ClickHouse, error) {
	db := clickhouse.OpenDB(&clickhouse.Options{
		Addr: cfg.Addrs,
		Auth: clickhouse.Auth{
			Database: cfg.Database,
			Username: cfg.Username,
			Password: cfg.Password,
		},
		DialTimeout: 5 * time.Second,
	})
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping clickhouse: %w", err)
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	ch := &ClickHouse{db: db, log: log}
	if err := ch.createTables(); err != nil {
		db.Close()
		return nil, fmt.Errorf("create tables: %w", err)
	}
	return ch, nil
}

var _ Store = (*ClickHouse)(nil)
var _ ObservabilityWriter = (*ClickHouse)(nil)
var _ RollupWriter = (*ClickHouse)(nil)

func (c *ClickHouse) createTables() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS impressions (
			trace_id String, insertion_order_id String, campaign_id String, creative_id String,
			placement_id String, publisher_id String, account_id String, geo String, device String,
			channel String, format String, clearing_price Float64, clearing_currency String,
			clearing_price_usd Float64, bid_model String, deal_id String,
			schema_version Int32 DEFAULT 1, timestamp DateTime64(3)
		) ENGINE = MergeTree ORDER BY timestamp`,
		`CREATE TABLE IF NOT EXISTS clicks (
			trace_id String, campaign_id String, creative_id String, placement_id String,
			publisher_id String, account_id String, landing_url String, geo String, device String,
			schema_version Int32 DEFAULT 1, timestamp DateTime64(3)
		) ENGINE = MergeTree ORDER BY timestamp`,
		`CREATE TABLE IF NOT EXISTS conversions (
			trace_id String, campaign_id String, creative_id String, placement_id String,
			account_id String, conversion_type String, revenue Float64, currency String,
			revenue_usd Float64, schema_version Int32 DEFAULT 1, timestamp DateTime64(3)
		) ENGINE = MergeTree ORDER BY timestamp`,
		`CREATE TABLE IF NOT EXISTS views (
			trace_id String, campaign_id String, creative_id String, placement_id String,
			publisher_id String, account_id String, duration_ms Int64, percent_visible Int32,
			area_px Int64, iab_viewable UInt8, schema_version Int32 DEFAULT 1, timestamp DateTime64(3)
		) ENGINE = MergeTree ORDER BY timestamp`,
		`CREATE TABLE IF NOT EXISTS auctions (
			trace_id String, placement_id String, publisher_id String, channel String, num_bids Int32,
			winning_bid Float64, clearing_price Float64, currency String, clearing_price_usd Float64,
			winner_dsp String, duration_ms Int64, deal_id String, schema_version Int32 DEFAULT 1,
			timestamp DateTime64(3)
		) ENGINE = MergeTree ORDER BY timestamp`,
		`CREATE TABLE IF NOT EXISTS auction_wins (
			trace_id String, auction_id String, winner_dsp String, campaign_id String, creative_id String,
			placement_id String, publisher_id String, advertiser_id String, clearing_price Float64,
			currency String, bid_model String, deal_id String, channel String,
			schema_version Int32 DEFAULT 1, timestamp DateTime64(3)
		) ENGINE = MergeTree ORDER BY timestamp`,
		`CREATE TABLE IF NOT EXISTS media_events (
			trace_id String, channel String, event_type String, position_ms Int64,
			schema_version Int32 DEFAULT 1, timestamp DateTime64(3)
		) ENGINE = MergeTree ORDER BY timestamp`,
		`CREATE TABLE IF NOT EXISTS serve_no_fills (
			trace_id String, publisher_id String, placement_id String, reason String, timestamp DateTime64(3)
		) ENGINE = MergeTree ORDER BY timestamp`,
		`CREATE TABLE IF NOT EXISTS freq_cap_blocks (
			trace_id String, user_id String, campaign_id String, placement_id String,
			publisher_id String, timestamp DateTime64(3)
		) ENGINE = MergeTree ORDER BY timestamp`,
		`CREATE TABLE IF NOT EXISTS render_failures (
			trace_id String, campaign_id String, creative_id String, placement_id String,
			publisher_id String, reason String, detail String, timestamp DateTime64(3)
		) ENGINE = MergeTree ORDER BY timestamp`,
		`CREATE TABLE IF NOT EXISTS tracker_rejections (
			trace_id String, event_type String, reason String, detail String, timestamp DateTime64(3)
		) ENGINE = MergeTree ORDER BY timestamp`,
		`CREATE TABLE IF NOT EXISTS campaign_state_changes (
			campaign_id String, account_id String, old_state String, new_state String,
			reason String, timestamp DateTime64(3)
		) ENGINE = MergeTree ORDER BY timestamp`,
		`CREATE TABLE IF NOT EXISTS budget_depletions (
			campaign_id String, account_id String, budget Float64, spent Float64, timestamp DateTime64(3)
		) ENGINE = MergeTree ORDER BY timestamp`,
		`CREATE TABLE IF NOT EXISTS rollups (
			config String, level String, window_from DateTime64(3), window_to DateTime64(3),
			dimensions String, metrics String, created_at DateTime64(3)
		) ENGINE = MergeTree ORDER BY (config, level, window_from)`,
	}
	for _, stmt := range statements {
		if _, err := c.db.Exec(stmt); err != nil {
			return fmt.Errorf("exec %.40s: %w", stmt, err)
		}
	}
	return nil
}

func b2u(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

func (c *ClickHouse) exec(ctx context.Context, kind, query string, args ...any) error {
	if _, err := c.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("insert %s: %w", kind, err)
	}
	return nil
}

func (c *ClickHouse) InsertImpression(ctx context.Context, e *ImpressionEvent) error {
	return c.exec(ctx, "impression",
		`INSERT INTO impressions (trace_id, insertion_order_id, campaign_id, creative_id, placement_id,
			publisher_id, account_id, geo, device, channel, format, clearing_price, clearing_currency,
			clearing_price_usd, bid_model, deal_id, schema_version, timestamp)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.TraceID, e.InsertionOrderID, e.CampaignID, e.CreativeID, e.PlacementID, e.PublisherID,
		e.AccountID, e.Geo, e.Device, e.Channel, e.Format, e.ClearingPrice, e.ClearingCurrency,
		e.ClearingPriceUSD, e.BidModel, e.DealID, int32(schemaVer(e.SchemaVersion)), e.Timestamp)
}

func (c *ClickHouse) InsertClick(ctx context.Context, e *ClickEvent) error {
	return c.exec(ctx, "click",
		`INSERT INTO clicks (trace_id, campaign_id, creative_id, placement_id, publisher_id, account_id,
			landing_url, geo, device, schema_version, timestamp) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		e.TraceID, e.CampaignID, e.CreativeID, e.PlacementID, e.PublisherID, e.AccountID,
		e.LandingURL, e.Geo, e.Device, int32(schemaVer(e.SchemaVersion)), e.Timestamp)
}

func (c *ClickHouse) InsertConversion(ctx context.Context, e *ConversionEvent) error {
	return c.exec(ctx, "conversion",
		`INSERT INTO conversions (trace_id, campaign_id, creative_id, placement_id, account_id,
			conversion_type, revenue, currency, revenue_usd, schema_version, timestamp)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		e.TraceID, e.CampaignID, e.CreativeID, e.PlacementID, e.AccountID, e.ConversionType,
		e.Revenue, e.Currency, e.RevenueUSD, int32(schemaVer(e.SchemaVersion)), e.Timestamp)
}

func (c *ClickHouse) InsertView(ctx context.Context, e *ViewEvent) error {
	return c.exec(ctx, "view",
		`INSERT INTO views (trace_id, campaign_id, creative_id, placement_id, publisher_id, account_id,
			duration_ms, percent_visible, area_px, iab_viewable, schema_version, timestamp)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.TraceID, e.CampaignID, e.CreativeID, e.PlacementID, e.PublisherID, e.AccountID,
		e.DurationMs, int32(e.PercentVisible), e.AreaPx, b2u(e.IABViewable), int32(schemaVer(e.SchemaVersion)), e.Timestamp)
}

func (c *ClickHouse) InsertAuction(ctx context.Context, e *AuctionEvent) error {
	return c.exec(ctx, "auction",
		`INSERT INTO auctions (trace_id, placement_id, publisher_id, channel, num_bids, winning_bid,
			clearing_price, currency, clearing_price_usd, winner_dsp, duration_ms, deal_id, schema_version, timestamp)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.TraceID, e.PlacementID, e.PublisherID, e.Channel, int32(e.NumBids), e.WinningBid,
		e.ClearingPrice, e.Currency, e.ClearingPriceUSD, e.WinnerDSP, e.DurationMs, e.DealID,
		int32(schemaVer(e.SchemaVersion)), e.Timestamp)
}

func (c *ClickHouse) InsertAuctionWin(ctx context.Context, e *AuctionWinEvent) error {
	return c.exec(ctx, "auction_win",
		`INSERT INTO auction_wins (trace_id, auction_id, winner_dsp, campaign_id, creative_id, placement_id,
			publisher_id, advertiser_id, clearing_price, currency, bid_model, deal_id, channel, schema_version, timestamp)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.TraceID, e.AuctionID, e.WinnerDSP, e.CampaignID, e.CreativeID, e.PlacementID, e.PublisherID,
		e.AdvertiserID, e.ClearingPrice, e.Currency, e.BidModel, e.DealID, e.Channel,
		int32(schemaVer(e.SchemaVersion)), e.Timestamp)
}

func (c *ClickHouse) InsertMediaEvent(ctx context.Context, e *MediaEvent) error {
	if e == nil {
		return nil
	}
	tsv := e.Timestamp
	if tsv.IsZero() {
		tsv = time.Now()
	}
	return c.exec(ctx, "media_event",
		`INSERT INTO media_events (trace_id, channel, event_type, position_ms, schema_version, timestamp)
		VALUES (?,?,?,?,?,?)`,
		e.TraceID, e.Channel, e.EventType, e.PositionMs, int32(1), tsv)
}

func (c *ClickHouse) InsertBatch(ctx context.Context, events []Event) error {
	for _, e := range events {
		var err error
		switch e.Type {
		case EventImpression:
			err = c.InsertImpression(ctx, e.Impression)
		case EventClick:
			err = c.InsertClick(ctx, e.Click)
		case EventConversion:
			err = c.InsertConversion(ctx, e.Conversion)
		case EventView:
			err = c.InsertView(ctx, e.View)
		case EventAuction:
			err = c.InsertAuction(ctx, e.Auction)
		default:
			return fmt.Errorf("unknown event type: %s", e.Type)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// --- Operational signals (ObservabilityWriter) — fire-and-forget, logged ---

func (c *ClickHouse) signal(kind, query string, args ...any) {
	if _, err := c.db.Exec(query, args...); err != nil {
		c.log.Error("clickhouse: operational-signal insert failed", "signal", kind, "error", err)
	}
}

func (c *ClickHouse) InsertServeNoFill(n ServeNoFill) {
	c.signal("serve_no_fill", `INSERT INTO serve_no_fills (trace_id, publisher_id, placement_id, reason, timestamp) VALUES (?,?,?,?,?)`,
		n.TraceID, n.PublisherID, n.PlacementID, n.Reason, sig(n.Timestamp))
}
func (c *ClickHouse) InsertFreqCapBlock(b FreqCapBlock) {
	c.signal("freq_cap_block", `INSERT INTO freq_cap_blocks (trace_id, user_id, campaign_id, placement_id, publisher_id, timestamp) VALUES (?,?,?,?,?,?)`,
		b.TraceID, b.UserID, b.CampaignID, b.PlacementID, b.PublisherID, sig(b.Timestamp))
}
func (c *ClickHouse) InsertRenderFailure(r RenderFailure) {
	c.signal("render_failure", `INSERT INTO render_failures (trace_id, campaign_id, creative_id, placement_id, publisher_id, reason, detail, timestamp) VALUES (?,?,?,?,?,?,?,?)`,
		r.TraceID, r.CampaignID, r.CreativeID, r.PlacementID, r.PublisherID, r.Reason, r.Detail, sig(r.Timestamp))
}
func (c *ClickHouse) InsertTrackerRejection(r TrackerRejection) {
	c.signal("tracker_rejection", `INSERT INTO tracker_rejections (trace_id, event_type, reason, detail, timestamp) VALUES (?,?,?,?,?)`,
		r.TraceID, r.EventType, r.Reason, r.Detail, sig(r.Timestamp))
}
func (c *ClickHouse) InsertCampaignStateChange(cs CampaignStateChange) {
	c.signal("campaign_state_change", `INSERT INTO campaign_state_changes (campaign_id, account_id, old_state, new_state, reason, timestamp) VALUES (?,?,?,?,?,?)`,
		cs.CampaignID, cs.AccountID, cs.OldState, cs.NewState, cs.Reason, sig(cs.Timestamp))
}
func (c *ClickHouse) InsertBudgetDepletion(b BudgetDepletion) {
	c.signal("budget_depletion", `INSERT INTO budget_depletions (campaign_id, account_id, budget, spent, timestamp) VALUES (?,?,?,?,?)`,
		b.CampaignID, b.AccountID, b.Budget, b.Spent, sig(b.Timestamp))
}

// InsertRollups persists aggregated rollup rows (RollupWriter). Idempotent by
// (config, level, window_from): a re-run replaces the window's prior rows via
// a lightweight DELETE (GA in ClickHouse ≥23.3) before inserting, matching the
// memory/DuckDB backends so retries/overlapping schedules don't double-count.
func (c *ClickHouse) InsertRollups(ctx context.Context, rows []RollupRow) error {
	if len(rows) == 0 {
		return nil
	}
	now := time.Now()

	seen := map[string]bool{}
	for _, r := range rows {
		k := r.Config + "\x00" + r.Level + "\x00" + r.WindowFrom.Format(time.RFC3339Nano)
		if seen[k] {
			continue
		}
		seen[k] = true
		if _, err := c.db.ExecContext(ctx,
			`DELETE FROM rollups WHERE config = ? AND level = ? AND window_from = ?`,
			r.Config, r.Level, r.WindowFrom); err != nil {
			return fmt.Errorf("delete prior rollups: %w", err)
		}
	}

	for _, r := range rows {
		dims, err := json.Marshal(r.Dimensions)
		if err != nil {
			return fmt.Errorf("marshal dimensions: %w", err)
		}
		mets, err := json.Marshal(r.Metrics)
		if err != nil {
			return fmt.Errorf("marshal metrics: %w", err)
		}
		if _, err := c.db.ExecContext(ctx,
			`INSERT INTO rollups (config, level, window_from, window_to, dimensions, metrics, created_at) VALUES (?,?,?,?,?,?,?)`,
			r.Config, r.Level, r.WindowFrom, r.WindowTo, string(dims), string(mets), now); err != nil {
			return fmt.Errorf("insert rollup: %w", err)
		}
	}
	return nil
}

func (c *ClickHouse) Query(ctx context.Context, params QueryParams) (*QueryResult, error) {
	query, args := BuildQuery(params)
	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("columns: %w", err)
	}
	result := &QueryResult{Columns: cols}
	for rows.Next() {
		vals := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		result.Rows = append(result.Rows, vals)
	}
	return result, rows.Err()
}

func (c *ClickHouse) Close() error { return c.db.Close() }

// sig backfills a zero timestamp so operational-signal rows always have a
// valid DateTime (the fire-and-forget call sites don't always set it).
func sig(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now()
	}
	return t
}

func schemaVer(v int) int {
	if v == 0 {
		return 1
	}
	return v
}
