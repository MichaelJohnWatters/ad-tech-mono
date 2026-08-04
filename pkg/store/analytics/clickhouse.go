package analytics

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"
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
	db      *sql.DB
	conn    clickhouse.Conn // native protocol — hot bulk-insert path (PrepareBatch)
	log     *slog.Logger
	ttlDays int
}

// ClickHouseConfig configures the connection. Addrs is host:port pairs.
type ClickHouseConfig struct {
	Addrs    []string
	Database string
	Username string
	Password string
	Log      *slog.Logger
	// TTLDays bounds the raw-event tables: rows older than this are dropped
	// by ClickHouse's own TTL (the hot tier stays small; the Delta lake is
	// the durable record). 0 = no TTL (unbounded). Rollup tables are exempt.
	TTLDays int
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

	// Native protocol connection alongside the database/sql handle. The
	// *sql.DB stays the path for reads, DDL, rollups and operational
	// signals (unchanged); conn is used only for the hot bulk-insert path,
	// where PrepareBatch is column-oriented and materially faster than the
	// row-at-a-time database/sql INSERT. Two pools to one server is fine.
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: cfg.Addrs,
		Auth: clickhouse.Auth{
			Database: cfg.Database,
			Username: cfg.Username,
			Password: cfg.Password,
		},
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("open clickhouse native conn: %w", err)
	}
	if err := conn.Ping(ctx); err != nil {
		db.Close()
		conn.Close()
		return nil, fmt.Errorf("ping clickhouse native conn: %w", err)
	}

	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	ch := &ClickHouse{db: db, conn: conn, log: log, ttlDays: cfg.TTLDays}
	if err := ch.createTables(); err != nil {
		db.Close()
		conn.Close()
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
			impression_qty Int32 DEFAULT 1,
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
			revenue_usd Float64, schema_version Int32 DEFAULT 1, timestamp DateTime64(3),
			attributed_trace_id String, attribution_type String, user_id String
		) ENGINE = MergeTree ORDER BY timestamp`,
		`CREATE TABLE IF NOT EXISTS views (
			trace_id String, campaign_id String, creative_id String, placement_id String,
			publisher_id String, account_id String, channel String, duration_ms Int64, percent_visible Int32,
			area_px Int64, iab_viewable UInt8, schema_version Int32 DEFAULT 1, timestamp DateTime64(3)
		) ENGINE = MergeTree ORDER BY timestamp`,
		// Multi-touch attribution chains (Phase 3): one row per exposure that
		// preceded a conversion. Model-agnostic — fractional credit is computed on
		// read (pkg/attribution). Reporting-only; billing settles last-touch.
		`CREATE TABLE IF NOT EXISTS attribution_touchpoints (
			conversion_trace_id String, touchpoint_trace_id String, account_id String,
			campaign_id String, touchpoint_type String, touchpoint_at DateTime64(3),
			conversion_at DateTime64(3), conversion_revenue Float64, observed_at DateTime64(3)
		) ENGINE = MergeTree ORDER BY (account_id, conversion_trace_id)`,
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
		`CREATE TABLE IF NOT EXISTS dsp_calls (
			trace_id String, auction_id String, channel String, dsp_endpoint String,
			bid_received UInt8, bid_price_usd Float64, latency_ms Int64, timed_out UInt8,
			no_bid_reason String, schema_version Int32 DEFAULT 1, timestamp DateTime64(3)
		) ENGINE = MergeTree ORDER BY timestamp`,
		// Profile-store tables (ADR 0006 phase 1) — landed in ClickHouse
		// ALONGSIDE the existing Parquet lake dual-write. Phase 2 repoints the
		// profile-builder's rule evaluation onto server-side GROUP BY over these.
		//
		// behaviour_signals: consent-gated behavioural observations. ORDER BY
		// (account_id, tag, user_id, observed_at) directly serves phase 2's hot
		// rule query "site_visit rows for a user/tag in a window" (account+tag
		// prefix, user + time as the trailing key). Partition by day so a window
		// scan only touches the relevant partitions. TTL 90d, NOT the raw-event
		// ttlDays (default 30d): behavioural rule windows run up to ~30d, so a
		// 30d hot TTL could evict a row a rule still needs mid-evaluation; 90d
		// gives headroom. The lake stays the forever copy.
		`CREATE TABLE IF NOT EXISTS behaviour_signals (
			trace_id String, kind String, user_id String, household_id String,
			placement_id String, publisher_id String, campaign_id String, creative_id String,
			channel String, categories String, geo String, device String,
			account_id String, tag String, observed_at DateTime64(3)
		) ENGINE = MergeTree
			PARTITION BY toYYYYMMDD(observed_at)
			ORDER BY (account_id, tag, user_id, observed_at)
			TTL toDateTime(observed_at) + INTERVAL 90 DAY`,
		// profile_signals: the EXPANDED one-row-per-id onboarding record. ORDER
		// BY (account_id, segment_id, id_value) serves "who is in this segment"
		// membership rebuilds. Partition by day of observation. TTL 365d (much
		// longer than behaviour) — onboarded lists are durable declared
		// memberships, not transient interaction signals, so they don't age out
		// the same way; the lake is still the keep-forever copy, this just keeps
		// a year of the onboarding record queryable in-engine.
		`CREATE TABLE IF NOT EXISTS profile_signals (
			trace_id String, ingest_trace_id String, account_id String, provider String, provider_id String, data_party String,
			source String, access String,
			segment_id String, segment_name String, visibility String, consent UInt8,
			id_type String, id_value String, observed_at DateTime64(3)
		) ENGINE = MergeTree
			PARTITION BY toYYYYMMDD(observed_at)
			ORDER BY (account_id, segment_id, id_value)
			TTL toDateTime(observed_at) + INTERVAL 365 DAY`,
		`CREATE TABLE IF NOT EXISTS rollups (
			config String, level String, window_from DateTime64(3), window_to DateTime64(3),
			dimensions String, metrics String, created_at DateTime64(3)
		) ENGINE = MergeTree ORDER BY (config, level, window_from)`,

		// Native, incrementally-maintained event rollups. SummingMergeTree
		// target tables + materialized views collapse impressions into hourly
		// and daily aggregates on insert — always fresh, no scheduler. The
		// reporting read path (QueryRollups) prefers these for the events
		// hourly/daily tiers; the app-side rollup engine still owns minute /
		// monthly / auctions. Dimensions mirror rollup.EventsConfig.
		// NB: MVs only capture rows inserted AFTER creation (insert-triggers);
		// a fresh local stack starts empty, which is the intended dev flow.
		`CREATE TABLE IF NOT EXISTS impressions_rollup_hourly (
			hour DateTime, account_id String, publisher_id String, campaign_id String,
			creative_id String, placement_id String, geo String, device String,
			count UInt64, sum_cost Float64
		) ENGINE = SummingMergeTree ORDER BY (hour, account_id, publisher_id, campaign_id, creative_id, placement_id, geo, device)`,
		// count = sum(impression_qty) so a DOOH play (impression_qty = venue
		// audience per play) rolls up as its audience, not 1 — consistent with the
		// raw query's count metric. DROP first so the redefinition lands on existing
		// stacks (CREATE IF NOT EXISTS alone keeps the old count() view); the
		// SummingMergeTree target table keeps its rows (new inserts use the new sum).
		`DROP VIEW IF EXISTS impressions_rollup_hourly_mv`,
		`CREATE MATERIALIZED VIEW IF NOT EXISTS impressions_rollup_hourly_mv TO impressions_rollup_hourly AS
			SELECT toStartOfHour(timestamp) AS hour, account_id, publisher_id, campaign_id, creative_id,
				placement_id, geo, device, sum(impression_qty) AS count, sum(clearing_price_usd) AS sum_cost
			FROM impressions
			GROUP BY hour, account_id, publisher_id, campaign_id, creative_id, placement_id, geo, device`,
		`CREATE TABLE IF NOT EXISTS impressions_rollup_daily (
			day DateTime, account_id String, publisher_id String, campaign_id String,
			creative_id String, placement_id String, geo String, device String,
			count UInt64, sum_cost Float64
		) ENGINE = SummingMergeTree ORDER BY (day, account_id, publisher_id, campaign_id, creative_id, placement_id, geo, device)`,
		`DROP VIEW IF EXISTS impressions_rollup_daily_mv`,
		`CREATE MATERIALIZED VIEW IF NOT EXISTS impressions_rollup_daily_mv TO impressions_rollup_daily AS
			SELECT toStartOfDay(timestamp) AS day, account_id, publisher_id, campaign_id, creative_id,
				placement_id, geo, device, sum(impression_qty) AS count, sum(clearing_price_usd) AS sum_cost
			FROM impressions
			GROUP BY day, account_id, publisher_id, campaign_id, creative_id, placement_id, geo, device`,
	}
	for _, stmt := range statements {
		if _, err := c.db.Exec(stmt); err != nil {
			return fmt.Errorf("exec %.40s: %w", stmt, err)
		}
	}
	// Additive columns on profile_signals for existing deployments (ADR 0009 —
	// data-provider provenance). CREATE TABLE IF NOT EXISTS above only shapes a
	// FRESH table; an already-created one needs ALTER. Non-fatal (older CH /
	// permissions shouldn't block boot); new rows carry the values, old rows read
	// as empty string — which is exactly "no provider / first party".
	for _, ddl := range []string{
		`ALTER TABLE profile_signals ADD COLUMN IF NOT EXISTS provider_id String`,
		`ALTER TABLE profile_signals ADD COLUMN IF NOT EXISTS data_party String`,
		// Batch-lineage correlation for uploaded rows ("ing_<32hex>" → the ingest
		// job). Distinct from trace_id (a real request trace); old rows read as
		// empty. See pkg/ingest.ingestJobTrace.
		`ALTER TABLE profile_signals ADD COLUMN IF NOT EXISTS ingest_trace_id String`,
		// Per-DSP no-bid reason (Phase H) — keeps a DSP-level enforcement block
		// (e.g. adcert_invalid) from being lost in the aggregated no-bid. Old rows
		// read as empty = "bid or plain no-demand".
		`ALTER TABLE dsp_calls ADD COLUMN IF NOT EXISTS no_bid_reason String`,
		// Video viewability: split display (1s dwell) vs video (2s) viewable
		// events. Old rows read as empty = display.
		`ALTER TABLE views ADD COLUMN IF NOT EXISTS channel String`,
		// Conversion attribution (Phase 0 — close the trace loop). The exposure
		// this conversion is credited to + how, and the advertiser-side visitor
		// id. Old rows read as empty = unattributed.
		`ALTER TABLE conversions ADD COLUMN IF NOT EXISTS attributed_trace_id String`,
		`ALTER TABLE conversions ADD COLUMN IF NOT EXISTS attribution_type String`,
		`ALTER TABLE conversions ADD COLUMN IF NOT EXISTS user_id String`,
		// DOOH audience multiplier: how many impressions one served event (proof-of-
		// play) represents. Old rows read as 0 → normalised to 1 on query (a served
		// event is at least one impression); new rows carry the venue audience.
		// AFTER deal_id so the column lands in the SAME position as a freshly
		// CREATE-d table — the batch insert appends by column order, so a mismatch
		// (ALTER defaults to appending at the end) would corrupt every batched row.
		`ALTER TABLE impressions ADD COLUMN IF NOT EXISTS impression_qty Int32 DEFAULT 1 AFTER deal_id`,
	} {
		if _, err := c.db.Exec(ddl); err != nil {
			c.log.Warn("clickhouse: could not add additive column", "ddl", ddl, "error", err)
		}
	}
	// Bound the raw-event tables: TTL drops rows older than ttlDays so the
	// HOT tier stays small (the Delta lake keeps everything; cold reads
	// serve older history). Applied to CREATE-time tables via ALTER so
	// existing deployments pick it up too. Rollup tables (ORDER BY (config
	// ...)) are exempt — small, long-lived aggregates. 0 = leave unbounded.
	if c.ttlDays > 0 {
		rawEventTables := []string{
			"impressions", "clicks", "conversions", "views", "auctions",
			"auction_wins", "media_events", "serve_no_fills", "freq_cap_blocks",
			"render_failures", "campaign_state_changes", "budget_depletions", "dsp_calls",
		}
		for _, t := range rawEventTables {
			ddl := fmt.Sprintf("ALTER TABLE %s MODIFY TTL toDateTime(timestamp) + INTERVAL %d DAY", t, c.ttlDays)
			if _, err := c.db.Exec(ddl); err != nil {
				// Non-fatal: a TTL that can't apply (older CH, permissions)
				// shouldn't stop the service booting — log and continue.
				c.log.Warn("clickhouse: could not apply TTL", "table", t, "days", c.ttlDays, "error", err)
			}
		}
		c.log.Info("clickhouse raw-event TTL applied", "days", c.ttlDays)
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
			clearing_price_usd, bid_model, deal_id, impression_qty, schema_version, timestamp)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.TraceID, e.InsertionOrderID, e.CampaignID, e.CreativeID, e.PlacementID, e.PublisherID,
		e.AccountID, e.Geo, e.Device, e.Channel, e.Format, e.ClearingPrice, e.ClearingCurrency,
		e.ClearingPriceUSD, e.BidModel, e.DealID, int32(impQty(e.ImpressionQty)), int32(schemaVer(e.SchemaVersion)), e.Timestamp)
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
			conversion_type, revenue, currency, revenue_usd, schema_version, timestamp,
			attributed_trace_id, attribution_type, user_id)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.TraceID, e.CampaignID, e.CreativeID, e.PlacementID, e.AccountID, e.ConversionType,
		e.Revenue, e.Currency, e.RevenueUSD, int32(schemaVer(e.SchemaVersion)), e.Timestamp,
		e.AttributedTraceID, e.AttributionType, e.UserID)
}

func (c *ClickHouse) InsertView(ctx context.Context, e *ViewEvent) error {
	return c.exec(ctx, "view",
		`INSERT INTO views (trace_id, campaign_id, creative_id, placement_id, publisher_id, account_id,
			channel, duration_ms, percent_visible, area_px, iab_viewable, schema_version, timestamp)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.TraceID, e.CampaignID, e.CreativeID, e.PlacementID, e.PublisherID, e.AccountID,
		e.Channel, e.DurationMs, int32(e.PercentVisible), e.AreaPx, b2u(e.IABViewable), int32(schemaVer(e.SchemaVersion)), e.Timestamp)
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

// PurgeFreqCapBlocks removes every freq-cap block row for the user — the
// Level-3 privacy deletion for the one operational-signal table that carries
// user_id. Lightweight DELETE (same as the rollups replace-by-window path):
// rows are masked from SELECTs immediately, physical removal is async.
func (c *ClickHouse) PurgeFreqCapBlocks(ctx context.Context, userID string) error {
	if _, err := c.db.ExecContext(ctx, `DELETE FROM freq_cap_blocks WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("purge freq_cap_blocks: %w", err)
	}
	return nil
}

// CountFreqCapBlocks counts the user's freq-cap block rows (privacy-verify
// residual check, and the purge's before-count — lightweight DELETE doesn't
// report affected rows).
func (c *ClickHouse) CountFreqCapBlocks(ctx context.Context, userID string) (int, error) {
	var n uint64
	if err := c.db.QueryRowContext(ctx, `SELECT count() FROM freq_cap_blocks WHERE user_id = ?`, userID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count freq_cap_blocks: %w", err)
	}
	return int(n), nil
}

// PurgeUserSignals removes the user's rows from the profile-store tables that
// carry user keys — behaviour_signals (user_id OR household_id) and
// profile_signals (id_value) — the Level-3 privacy deletion for the ClickHouse
// copies these gained in ADR 0006 phase 1. Lightweight DELETE (mutations_sync=1
// so the rows are gone before any re-export re-derives the partition). The
// caller re-exports affected hours so the derived Parquet loses them too.
func (c *ClickHouse) PurgeUserSignals(ctx context.Context, userID string) error {
	if _, err := c.db.ExecContext(ctx,
		`DELETE FROM behaviour_signals WHERE user_id = ? OR household_id = ? SETTINGS mutations_sync = 1`,
		userID, userID); err != nil {
		return fmt.Errorf("purge behaviour_signals: %w", err)
	}
	if _, err := c.db.ExecContext(ctx,
		`DELETE FROM profile_signals WHERE id_value = ? SETTINGS mutations_sync = 1`,
		userID); err != nil {
		return fmt.Errorf("purge profile_signals: %w", err)
	}
	return nil
}

// CountUserSignals counts the user's rows across behaviour_signals +
// profile_signals (privacy-verify residual check + the purge's before-count,
// since lightweight DELETE doesn't report affected rows).
func (c *ClickHouse) CountUserSignals(ctx context.Context, userID string) (behaviour, profile int, err error) {
	var b, p uint64
	if err = c.db.QueryRowContext(ctx,
		`SELECT count() FROM behaviour_signals WHERE user_id = ? OR household_id = ?`, userID, userID).Scan(&b); err != nil {
		return 0, 0, fmt.Errorf("count behaviour_signals: %w", err)
	}
	if err = c.db.QueryRowContext(ctx,
		`SELECT count() FROM profile_signals WHERE id_value = ?`, userID).Scan(&p); err != nil {
		return 0, 0, fmt.Errorf("count profile_signals: %w", err)
	}
	return int(b), int(p), nil
}

// AffectedSignalHours returns the distinct hours (by observed_at) in which the
// user appears across behaviour_signals + profile_signals — the exact set of
// Parquet export partitions that must be re-derived after a delete so the
// archive loses the user too. Queried BEFORE the delete.
func (c *ClickHouse) AffectedSignalHours(ctx context.Context, userID string) ([]time.Time, error) {
	const q = `SELECT DISTINCT toStartOfHour(observed_at) AS h FROM (
		SELECT observed_at FROM behaviour_signals WHERE user_id = ? OR household_id = ?
		UNION ALL
		SELECT observed_at FROM profile_signals WHERE id_value = ?
	) ORDER BY h`
	rows, err := c.db.QueryContext(ctx, q, userID, userID, userID)
	if err != nil {
		return nil, fmt.Errorf("affected signal hours: %w", err)
	}
	defer rows.Close()
	var hours []time.Time
	for rows.Next() {
		var h time.Time
		if err := rows.Scan(&h); err != nil {
			return nil, fmt.Errorf("scan affected hour: %w", err)
		}
		hours = append(hours, h.UTC())
	}
	return hours, rows.Err()
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

// QueryRollups reads persisted rollup rows for (config, level) whose window
// overlaps [from, to] (RollupReader). For the events hourly/daily tiers it
// reads the native SummingMergeTree materialized views (always-fresh,
// maintained on insert); everything else reads the generic `rollups` table
// populated by the app-side rollup engine.
func (c *ClickHouse) QueryRollups(ctx context.Context, config, level string, from, to time.Time) ([]RollupRow, error) {
	if config == "events" && (level == "hourly" || level == "daily") {
		return c.queryEventsMV(ctx, level, from, to)
	}
	q := `SELECT config, level, window_from, window_to, dimensions, metrics FROM rollups WHERE config = ? AND level = ?`
	args := []any{config, level}
	if !to.IsZero() {
		q += ` AND window_from < ?`
		args = append(args, to)
	}
	if !from.IsZero() {
		q += ` AND window_to > ?`
		args = append(args, from)
	}
	rows, err := c.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query rollups: %w", err)
	}
	defer rows.Close()
	var out []RollupRow
	for rows.Next() {
		var r RollupRow
		var dims, mets string
		if err := rows.Scan(&r.Config, &r.Level, &r.WindowFrom, &r.WindowTo, &dims, &mets); err != nil {
			return nil, fmt.Errorf("scan rollup: %w", err)
		}
		if err := json.Unmarshal([]byte(dims), &r.Dimensions); err != nil {
			return nil, fmt.Errorf("decode rollup dimensions: %w", err)
		}
		if err := json.Unmarshal([]byte(mets), &r.Metrics); err != nil {
			return nil, fmt.Errorf("decode rollup metrics: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CommittedByCampaign implements CommittedReader: realized committed spend per
// campaign for the UTC day, in micro-dollars, summed from the raw impression
// stream. clearing_price_usd is per-impression cost (the tracker converts the
// CPM at source), so summing it over a campaign's CPM impressions is that
// campaign's realized spend — independent of how many reporting replicas
// ingested the events, which is exactly what the shared pacing counter's
// reconcile needs. Covers CPM (billed on impression, the dominant path);
// reserve/settle models realize on their trigger event and are carried between
// reconciles by the additive delta path plus the DSP's local over-count guard.
func (c *ClickHouse) CommittedByCampaign(ctx context.Context, day string) (map[string]int64, error) {
	q := `SELECT campaign_id, sum(clearing_price_usd) AS spend_usd
		FROM impressions
		WHERE toDate(timestamp, 'UTC') = ? AND (bid_model = 'cpm' OR bid_model = '')
		GROUP BY campaign_id`
	rows, err := c.db.QueryContext(ctx, q, day)
	if err != nil {
		return nil, fmt.Errorf("committed by campaign: %w", err)
	}
	defer rows.Close()
	out := make(map[string]int64)
	for rows.Next() {
		var cid string
		var spendUSD float64
		if err := rows.Scan(&cid, &spendUSD); err != nil {
			return nil, fmt.Errorf("scan committed: %w", err)
		}
		if cid == "" {
			continue
		}
		out[cid] = int64(math.Round(spendUSD * 1_000_000))
	}
	return out, rows.Err()
}

// ImpressionsByPublisher returns publisher_id -> impression count since `since`
// (typically the 1st of the current UTC month), across every reporting replica
// — the cluster-global count the tiered revenue-share fee is chosen off (a
// publisher crossing a volume tier mid-month gets the better split). Counts all
// bid models: the tier is about supply volume, not billing model.
func (c *ClickHouse) ImpressionsByPublisher(ctx context.Context, since time.Time) (map[string]int64, error) {
	q := `SELECT publisher_id, count() AS n
		FROM impressions
		WHERE timestamp >= ?
		GROUP BY publisher_id`
	rows, err := c.db.QueryContext(ctx, q, since.UTC())
	if err != nil {
		return nil, fmt.Errorf("impressions by publisher: %w", err)
	}
	defer rows.Close()
	out := make(map[string]int64)
	for rows.Next() {
		var pub string
		var n int64
		if err := rows.Scan(&pub, &n); err != nil {
			return nil, fmt.Errorf("scan impressions by publisher: %w", err)
		}
		if pub == "" {
			continue
		}
		out[pub] = n
	}
	return out, rows.Err()
}

// InsertAttributionTouchpoints bulk-writes a conversion's multi-touch chain.
func (c *ClickHouse) InsertAttributionTouchpoints(ctx context.Context, rows []*AttributionTouchpointRow) error {
	if len(rows) == 0 {
		return nil
	}
	b, err := c.conn.PrepareBatch(ctx, `INSERT INTO attribution_touchpoints
		(conversion_trace_id, touchpoint_trace_id, account_id, campaign_id, touchpoint_type,
		 touchpoint_at, conversion_at, conversion_revenue, observed_at)`)
	if err != nil {
		return fmt.Errorf("prepare attribution_touchpoints batch: %w", err)
	}
	for _, r := range rows {
		if err := b.Append(r.ConversionTraceID, r.TouchpointTraceID, r.AccountID, r.CampaignID,
			r.TouchpointType, r.TouchpointAt, r.ConversionAt, r.ConversionRevenue, r.ObservedAt); err != nil {
			b.Abort()
			return fmt.Errorf("append attribution touchpoint: %w", err)
		}
	}
	return b.Send()
}

// AttributionChain reads a conversion's stored multi-touch chain, oldest first.
func (c *ClickHouse) AttributionChain(ctx context.Context, conversionTraceID string) ([]AttributionTouchpointRow, error) {
	if conversionTraceID == "" {
		return nil, nil
	}
	q := `SELECT conversion_trace_id, touchpoint_trace_id, account_id, campaign_id, touchpoint_type,
			touchpoint_at, conversion_at, conversion_revenue, observed_at
		FROM attribution_touchpoints WHERE conversion_trace_id = ? ORDER BY touchpoint_at ASC`
	rows, err := c.db.QueryContext(ctx, q, conversionTraceID)
	if err != nil {
		return nil, fmt.Errorf("attribution chain: %w", err)
	}
	defer rows.Close()
	var out []AttributionTouchpointRow
	for rows.Next() {
		var r AttributionTouchpointRow
		if err := rows.Scan(&r.ConversionTraceID, &r.TouchpointTraceID, &r.AccountID, &r.CampaignID,
			&r.TouchpointType, &r.TouchpointAt, &r.ConversionAt, &r.ConversionRevenue, &r.ObservedAt); err != nil {
			return nil, fmt.Errorf("scan attribution touchpoint: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ViewableImpressionsForUsers finds the ad exposures a click-less (view-through)
// conversion can be credited to. Source is behaviour_signals (kind='impression')
// — it carries the consented user_id the impressions table lacks — LEFT JOINed to
// the views table for the IAB-viewable verdict. Scoped to one advertiser account,
// optionally one campaign, since a lookback point; most-recent first.
func (c *ClickHouse) ViewableImpressionsForUsers(ctx context.Context, userIDs []string, accountID, campaignID string, since time.Time, requireViewable bool) ([]ViewableImpression, error) {
	if len(userIDs) == 0 || accountID == "" {
		return nil, nil
	}
	// The resolved id set can hold user ids AND household ids (the identity graph
	// links both). Match either column so a household-linked exposure counts when
	// the exact user id doesn't line up (cross-device / CTV fallback).
	ph := make([]string, len(userIDs))
	args := []any{accountID, since.UTC()}
	for i, u := range userIDs {
		ph[i] = "?"
		args = append(args, u)
	}
	inList := strings.Join(ph, ",")
	// user id set appears twice (user_id IN … OR household_id IN …).
	for _, u := range userIDs {
		args = append(args, u)
	}
	q := `SELECT b.trace_id, b.campaign_id, b.observed_at, if(v.viewable > 0, 1, 0) AS viewable
		FROM behaviour_signals AS b
		LEFT JOIN (SELECT trace_id, max(iab_viewable) AS viewable FROM views GROUP BY trace_id) AS v
		  ON v.trace_id = b.trace_id
		WHERE b.kind = 'impression' AND b.account_id = ? AND b.observed_at >= ?
		  AND (b.user_id IN (` + inList + `) OR b.household_id IN (` + inList + `))`
	if campaignID != "" {
		q += ` AND b.campaign_id = ?`
		args = append(args, campaignID)
	}
	if requireViewable {
		q += ` AND v.viewable = 1`
	}
	q += ` ORDER BY b.observed_at DESC`

	rows, err := c.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("viewable impressions for users: %w", err)
	}
	defer rows.Close()
	var out []ViewableImpression
	for rows.Next() {
		var r ViewableImpression
		var viewable uint8
		if err := rows.Scan(&r.TraceID, &r.CampaignID, &r.Timestamp, &viewable); err != nil {
			return nil, fmt.Errorf("scan viewable impression: %w", err)
		}
		r.Viewable = viewable == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// queryEventsMV reads the native impression rollup materialized view for the
// given tier and maps each bucket to a RollupRow the builder re-aggregates
// like any other rollup. SummingMergeTree rows may be partially merged, so we
// GROUP BY + sum() to get the final per-bucket totals.
func (c *ClickHouse) queryEventsMV(ctx context.Context, level string, from, to time.Time) ([]RollupRow, error) {
	table, tcol, step := "impressions_rollup_hourly", "hour", time.Hour
	if level == "daily" {
		table, tcol, step = "impressions_rollup_daily", "day", 24*time.Hour
	}
	q := `SELECT ` + tcol + `, account_id, publisher_id, campaign_id, creative_id, placement_id, geo, device,
		sum(count) AS count, sum(sum_cost) AS sum_cost FROM ` + table + ` WHERE 1=1`
	args := []any{}
	if !to.IsZero() {
		q += ` AND ` + tcol + ` < ?`
		args = append(args, to)
	}
	if !from.IsZero() {
		// bucket_end = bucket_start + step must be after `from` to overlap.
		q += ` AND ` + tcol + ` + INTERVAL ? SECOND > ?`
		args = append(args, int64(step.Seconds()), from)
	}
	q += ` GROUP BY ` + tcol + `, account_id, publisher_id, campaign_id, creative_id, placement_id, geo, device`

	rows, err := c.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query events rollup MV: %w", err)
	}
	defer rows.Close()
	var out []RollupRow
	for rows.Next() {
		var bucket time.Time
		var accountID, publisherID, campaignID, creativeID, placementID, geo, device string
		var count uint64
		var sumCost float64
		if err := rows.Scan(&bucket, &accountID, &publisherID, &campaignID, &creativeID, &placementID, &geo, &device, &count, &sumCost); err != nil {
			return nil, fmt.Errorf("scan events rollup MV: %w", err)
		}
		out = append(out, RollupRow{
			Config: "events", Level: level, WindowFrom: bucket, WindowTo: bucket.Add(step),
			Dimensions: map[string]string{
				"account_id": accountID, "publisher_id": publisherID, "campaign_id": campaignID,
				"creative_id": creativeID, "placement_id": placementID, "geo": geo, "device": device,
			},
			Metrics: map[string]float64{"count": float64(count), "sum_cost": sumCost},
		})
	}
	return out, rows.Err()
}

// CreativeStats aggregates impressions + clicks per creative since a cutoff
// (CreativeStatAggregator) — the ad server's bandit warm-start source. Two
// GROUP BYs joined on creative_id so a creative with impressions but no clicks
// still appears.
func (c *ClickHouse) CreativeStats(ctx context.Context, since time.Time) ([]CreativeStat, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT creative_id, sum(imps) AS imps, sum(clk) AS clk FROM (
			SELECT creative_id, count() AS imps, 0 AS clk FROM impressions WHERE timestamp >= ? GROUP BY creative_id
			UNION ALL
			SELECT creative_id, 0 AS imps, count() AS clk FROM clicks WHERE timestamp >= ? GROUP BY creative_id
		) GROUP BY creative_id`, since, since)
	if err != nil {
		return nil, fmt.Errorf("creative stats: %w", err)
	}
	defer rows.Close()
	var out []CreativeStat
	for rows.Next() {
		var s CreativeStat
		if err := rows.Scan(&s.CreativeID, &s.Impressions, &s.Clicks); err != nil {
			return nil, fmt.Errorf("scan creative stat: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// DSPCallStats aggregates dsp_calls per (channel, endpoint) since a cutoff
// (DSPCallAggregator) — the exchange's routing warm-start source.
//
// ifNotFinite: avgIf over a group with ZERO bids is NaN, and one NaN
// anywhere poisons the whole response — json.Encode rejects NaN before
// writing a single byte, so the routing-stats endpoint returned a silent
// 200-with-empty-body and the exchange warm-start/reseed decoded EOF. An
// always-no-bid DSP (the exact case smart routing exists to learn) hit it
// every time. The Go-side scrub is belt-and-braces for other backends.
func (c *ClickHouse) DSPCallStats(ctx context.Context, since time.Time) ([]DSPCallStat, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT channel, dsp_endpoint, count() AS calls,
			sum(bid_received) AS bids, sum(timed_out) AS timeouts,
			ifNotFinite(avgIf(bid_price_usd, bid_received = 1), 0) AS avg_bid,
			ifNotFinite(avg(latency_ms), 0) AS avg_lat
		 FROM dsp_calls WHERE timestamp >= ?
		 GROUP BY channel, dsp_endpoint`, since)
	if err != nil {
		return nil, fmt.Errorf("dsp_call stats: %w", err)
	}
	defer rows.Close()
	var out []DSPCallStat
	for rows.Next() {
		var s DSPCallStat
		var avgBid, avgLat float64
		if err := rows.Scan(&s.Channel, &s.DSPEndpoint, &s.TotalCalls, &s.TotalBids, &s.TotalTimeouts, &avgBid, &avgLat); err != nil {
			return nil, fmt.Errorf("scan dsp_call stat: %w", err)
		}
		if math.IsNaN(avgBid) || math.IsInf(avgBid, 0) {
			avgBid = 0
		}
		if math.IsNaN(avgLat) || math.IsInf(avgLat, 0) {
			avgLat = 0
		}
		s.AvgBidUSD = avgBid
		s.AvgLatencyMs = avgLat
		out = append(out, s)
	}
	return out, rows.Err()
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

func (c *ClickHouse) Close() error {
	err := c.db.Close()
	if c.conn != nil {
		if cerr := c.conn.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	return err
}

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

// impQty normalises the impression audience multiplier: a served event is at
// least one impression, so 0 (unset / legacy) becomes 1. DOOH carries the venue
// audience per play.
func impQty(v int) int {
	if v <= 0 {
		return 1
	}
	return v
}
