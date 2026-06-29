//go:build duckdb

package analytics

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"time"

	_ "github.com/marcboeker/go-duckdb"
)

// DuckDB is an embedded DuckDB analytics store.
// Zero infrastructure - just a file on disk.
// Used for local development and staging.
type DuckDB struct {
	db  *sql.DB
	log *slog.Logger
	mu  sync.Mutex // DuckDB writes are single-threaded
}

var _ ObservabilityWriter = (*DuckDB)(nil)

// NewDuckDB opens (or creates) a DuckDB database at the given path.
// Use ":memory:" for an in-memory database (testing).
func NewDuckDB(path string) (*DuckDB, error) {
	db, err := sql.Open("duckdb", path)
	if err != nil {
		return nil, fmt.Errorf("open duckdb %s: %w", path, err)
	}

	store := &DuckDB{db: db, log: slog.Default()}
	if err := store.createTables(); err != nil {
		db.Close()
		return nil, fmt.Errorf("create tables: %w", err)
	}
	return store, nil
}

// SetLogger sets the logger used to report operational-signal write
// failures (those methods are fire-and-forget, so a failed insert is
// logged rather than returned). No-op for a nil logger.
func (d *DuckDB) SetLogger(log *slog.Logger) {
	if log != nil {
		d.log = log
	}
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
		`CREATE TABLE IF NOT EXISTS views (
			trace_id        VARCHAR NOT NULL,
			campaign_id     VARCHAR NOT NULL,
			creative_id     VARCHAR,
			placement_id    VARCHAR NOT NULL,
			publisher_id    VARCHAR NOT NULL,
			account_id      VARCHAR NOT NULL,
			duration_ms     BIGINT NOT NULL,
			percent_visible INTEGER NOT NULL,
			area_px         BIGINT,
			iab_viewable    BOOLEAN NOT NULL,
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
		// auction_wins is the row-per-win projection of an auction —
		// the financial signal that bills downstream. Distinct from
		// `auctions` (one row per auction, including no-bids) because
		// queries that compute spend/CPM should join on this table
		// alone. Populated by exchange's AuctionWinEvent, pubad's
		// DirectWinEvent + PrebidOutboundWinEvent — every wire shape
		// maps onto this row.
		`CREATE TABLE IF NOT EXISTS auction_wins (
			trace_id        VARCHAR NOT NULL,
			auction_id      VARCHAR,
			winner_dsp      VARCHAR,
			campaign_id     VARCHAR,
			creative_id     VARCHAR,
			placement_id    VARCHAR,
			publisher_id    VARCHAR,
			advertiser_id   VARCHAR,
			clearing_price  DOUBLE,
			currency        VARCHAR,
			bid_model       VARCHAR,
			deal_id         VARCHAR,
			channel         VARCHAR,
			schema_version  INTEGER DEFAULT 1,
			timestamp       TIMESTAMP NOT NULL
		)`,
		// One row per player-fired engagement (start / firstQuartile /
		// midpoint / thirdQuartile / complete / mute / pause / resume /
		// skip / fullscreen for video; analogous events for audio).
		// Channel column distinguishes video vs audio so a single
		// table serves both consumers — keeps the rollup queries
		// uniform (the channel column is just another filter).
		// position_ms is the player position when the event fired
		// (when the SDK reports it; some events leave it 0).
		`CREATE TABLE IF NOT EXISTS media_events (
			trace_id       VARCHAR NOT NULL,
			channel        VARCHAR NOT NULL,
			event_type     VARCHAR NOT NULL,
			position_ms    BIGINT,
			schema_version INTEGER DEFAULT 1,
			timestamp      TIMESTAMP NOT NULL
		)`,
		// Operational-signal tables — non-bid state transitions surfaced to
		// ops dashboards (see ObservabilityWriter). MemoryStore keeps these
		// as slices; here they're real tables so the signals survive restart
		// on the durable backend instead of being dropped.
		`CREATE TABLE IF NOT EXISTS serve_no_fills (
			trace_id     VARCHAR NOT NULL,
			publisher_id VARCHAR,
			placement_id VARCHAR,
			reason       VARCHAR,
			timestamp    TIMESTAMP NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS freq_cap_blocks (
			trace_id     VARCHAR NOT NULL,
			user_id      VARCHAR,
			campaign_id  VARCHAR,
			placement_id VARCHAR,
			publisher_id VARCHAR,
			timestamp    TIMESTAMP NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS render_failures (
			trace_id     VARCHAR NOT NULL,
			campaign_id  VARCHAR,
			creative_id  VARCHAR,
			placement_id VARCHAR,
			publisher_id VARCHAR,
			reason       VARCHAR,
			detail       VARCHAR,
			timestamp    TIMESTAMP NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS tracker_rejections (
			trace_id   VARCHAR NOT NULL,
			event_type VARCHAR,
			reason     VARCHAR,
			detail     VARCHAR,
			timestamp  TIMESTAMP NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS campaign_state_changes (
			campaign_id VARCHAR NOT NULL,
			account_id  VARCHAR,
			old_state   VARCHAR,
			new_state   VARCHAR,
			reason      VARCHAR,
			timestamp   TIMESTAMP NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS budget_depletions (
			campaign_id VARCHAR NOT NULL,
			account_id  VARCHAR,
			budget      DOUBLE,
			spent       DOUBLE,
			timestamp   TIMESTAMP NOT NULL
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

func (d *DuckDB) InsertView(ctx context.Context, e *ViewEvent) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	_, err := d.db.ExecContext(ctx,
		`INSERT INTO views (trace_id, campaign_id, creative_id, placement_id, publisher_id,
			account_id, duration_ms, percent_visible, area_px, iab_viewable,
			schema_version, timestamp)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.TraceID, e.CampaignID, e.CreativeID, e.PlacementID, e.PublisherID,
		e.AccountID, e.DurationMs, e.PercentVisible, e.AreaPx, e.IABViewable,
		e.SchemaVersion, e.Timestamp)
	return err
}

// InsertMediaEvent writes a video / audio engagement row. Reporting
// calls this for every adtech.events.video / .audio NATS message;
// without it those events were silently acked + dropped in DuckDB
// deployments (only MemoryStore persisted them).
func (d *DuckDB) InsertMediaEvent(ctx context.Context, e *MediaEvent) error {
	if e == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	ts := e.Timestamp
	if ts.IsZero() {
		// Defensive: reporting backfills now() when the publisher left
		// the timestamp zero, but write paths should keep that
		// invariant too so a queried row always has a valid time.
		ts = time.Now()
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO media_events (trace_id, channel, event_type, position_ms, schema_version, timestamp)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		e.TraceID, e.Channel, e.EventType, e.PositionMs, 1, ts)
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

// InsertAuctionWin records a winning bid into the `auction_wins`
// table. Three handlers in cmd/reporting (handleAuctionWin,
// handleDirectWin, handlePrebidOutboundWin) all land here — every
// wire-shape gets normalised onto the AuctionWinEvent struct before
// arriving. Before this was wired, all three calls silently dropped
// on the DuckDB backend; analytics queries on auction_wins returned
// empty results even though events flowed.
func (d *DuckDB) InsertAuctionWin(ctx context.Context, e *AuctionWinEvent) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO auction_wins (
			trace_id, auction_id, winner_dsp, campaign_id, creative_id,
			placement_id, publisher_id, advertiser_id, clearing_price,
			currency, bid_model, deal_id, channel, schema_version, timestamp
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		e.TraceID, e.AuctionID, e.WinnerDSP, e.CampaignID, e.CreativeID,
		e.PlacementID, e.PublisherID, e.AdvertiserID, e.ClearingPrice,
		e.Currency, e.BidModel, e.DealID, e.Channel, e.SchemaVersion, e.Timestamp)
	return err
}

// execSignal runs a fire-and-forget operational-signal insert. These
// methods match MemoryStore's no-error signatures (the call sites can't do
// anything useful with the error), so a failed write is logged at ERROR
// rather than returned — visible to ops, non-fatal to the event pipeline.
func (d *DuckDB) execSignal(kind, query string, args ...any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ts := time.Now()
	for i, a := range args {
		if t, ok := a.(time.Time); ok && t.IsZero() {
			args[i] = ts
		}
	}
	if _, err := d.db.Exec(query, args...); err != nil {
		d.log.Error("duckdb: operational-signal insert failed", "signal", kind, "error", err)
	}
}

func (d *DuckDB) InsertServeNoFill(n ServeNoFill) {
	d.execSignal("serve_no_fill",
		`INSERT INTO serve_no_fills (trace_id, publisher_id, placement_id, reason, timestamp)
		 VALUES (?, ?, ?, ?, ?)`,
		n.TraceID, n.PublisherID, n.PlacementID, n.Reason, n.Timestamp)
}

func (d *DuckDB) InsertFreqCapBlock(b FreqCapBlock) {
	d.execSignal("freq_cap_block",
		`INSERT INTO freq_cap_blocks (trace_id, user_id, campaign_id, placement_id, publisher_id, timestamp)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		b.TraceID, b.UserID, b.CampaignID, b.PlacementID, b.PublisherID, b.Timestamp)
}

func (d *DuckDB) InsertRenderFailure(r RenderFailure) {
	d.execSignal("render_failure",
		`INSERT INTO render_failures (trace_id, campaign_id, creative_id, placement_id, publisher_id, reason, detail, timestamp)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.TraceID, r.CampaignID, r.CreativeID, r.PlacementID, r.PublisherID, r.Reason, r.Detail, r.Timestamp)
}

func (d *DuckDB) InsertTrackerRejection(r TrackerRejection) {
	d.execSignal("tracker_rejection",
		`INSERT INTO tracker_rejections (trace_id, event_type, reason, detail, timestamp)
		 VALUES (?, ?, ?, ?, ?)`,
		r.TraceID, r.EventType, r.Reason, r.Detail, r.Timestamp)
}

func (d *DuckDB) InsertCampaignStateChange(c CampaignStateChange) {
	d.execSignal("campaign_state_change",
		`INSERT INTO campaign_state_changes (campaign_id, account_id, old_state, new_state, reason, timestamp)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		c.CampaignID, c.AccountID, c.OldState, c.NewState, c.Reason, c.Timestamp)
}

func (d *DuckDB) InsertBudgetDepletion(b BudgetDepletion) {
	d.execSignal("budget_depletion",
		`INSERT INTO budget_depletions (campaign_id, account_id, budget, spent, timestamp)
		 VALUES (?, ?, ?, ?, ?)`,
		b.CampaignID, b.AccountID, b.Budget, b.Spent, b.Timestamp)
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
		case EventView:
			err = d.InsertView(ctx, e.View)
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
