package analytics

import (
	"context"
	"fmt"
	"time"
)

// Bulk-insert methods (BatchInserter). Each opens a native PrepareBatch,
// appends every row in DDL column order, and Sends once — a single atomic
// MergeTree block insert. Send() is all-or-nothing: on error zero rows are
// committed, which is what lets the reporting batch consumer ack-all or
// nak-all with no partial writes. Append order MUST match the CREATE TABLE
// column order in createTables (PrepareBatch binds positionally).

var _ BatchInserter = (*ClickHouse)(nil)

func bts(t time.Time) time.Time { // backfill a zero timestamp
	if t.IsZero() {
		return time.Now()
	}
	return t
}

func (c *ClickHouse) InsertImpressions(ctx context.Context, es []*ImpressionEvent) error {
	if len(es) == 0 {
		return nil
	}
	b, err := c.conn.PrepareBatch(ctx, "INSERT INTO impressions")
	if err != nil {
		return fmt.Errorf("prepare impressions batch: %w", err)
	}
	for _, e := range es {
		if err := b.Append(
			e.TraceID, e.InsertionOrderID, e.CampaignID, e.CreativeID, e.PlacementID,
			e.PublisherID, e.AccountID, e.Geo, e.Device, e.Channel, e.Format,
			e.ClearingPrice, e.ClearingCurrency, e.ClearingPriceUSD, e.BidModel, e.DealID,
			int32(impQty(e.ImpressionQty)), int32(schemaVer(e.SchemaVersion)), bts(e.Timestamp),
		); err != nil {
			b.Abort()
			return fmt.Errorf("append impression: %w", err)
		}
	}
	return b.Send()
}

func (c *ClickHouse) InsertClicks(ctx context.Context, es []*ClickEvent) error {
	if len(es) == 0 {
		return nil
	}
	b, err := c.conn.PrepareBatch(ctx, "INSERT INTO clicks")
	if err != nil {
		return fmt.Errorf("prepare clicks batch: %w", err)
	}
	for _, e := range es {
		if err := b.Append(
			e.TraceID, e.CampaignID, e.CreativeID, e.PlacementID, e.PublisherID, e.AccountID,
			e.LandingURL, e.Geo, e.Device, int32(schemaVer(e.SchemaVersion)), bts(e.Timestamp),
		); err != nil {
			b.Abort()
			return fmt.Errorf("append click: %w", err)
		}
	}
	return b.Send()
}

func (c *ClickHouse) InsertConversions(ctx context.Context, es []*ConversionEvent) error {
	if len(es) == 0 {
		return nil
	}
	// Explicit named columns (not positional): the attribution columns were
	// added by ALTER on existing tables (appended LAST) but sit at CREATE-time
	// position on a fresh table, so a column-less INSERT would bind by the wrong
	// order. Named columns bind by name regardless (same reason as views).
	b, err := c.conn.PrepareBatch(ctx, `INSERT INTO conversions
		(trace_id, campaign_id, creative_id, placement_id, account_id, conversion_type,
		 revenue, currency, revenue_usd, schema_version, timestamp,
		 attributed_trace_id, attribution_type, user_id)`)
	if err != nil {
		return fmt.Errorf("prepare conversions batch: %w", err)
	}
	for _, e := range es {
		if err := b.Append(
			e.TraceID, e.CampaignID, e.CreativeID, e.PlacementID, e.AccountID, e.ConversionType,
			e.Revenue, e.Currency, e.RevenueUSD, int32(schemaVer(e.SchemaVersion)), bts(e.Timestamp),
			e.AttributedTraceID, e.AttributionType, e.UserID,
		); err != nil {
			b.Abort()
			return fmt.Errorf("append conversion: %w", err)
		}
	}
	return b.Send()
}

func (c *ClickHouse) InsertViews(ctx context.Context, es []*ViewEvent) error {
	if len(es) == 0 {
		return nil
	}
	// Explicit column list (not positional): `channel` was added by ALTER on
	// existing tables (appended LAST) but sits mid-DDL on a fresh table, so a
	// column-less INSERT would bind by the wrong order. Named columns bind by
	// name regardless.
	b, err := c.conn.PrepareBatch(ctx, `INSERT INTO views
		(trace_id, campaign_id, creative_id, placement_id, publisher_id, account_id,
		 channel, duration_ms, percent_visible, area_px, iab_viewable, schema_version, timestamp)`)
	if err != nil {
		return fmt.Errorf("prepare views batch: %w", err)
	}
	for _, e := range es {
		if err := b.Append(
			e.TraceID, e.CampaignID, e.CreativeID, e.PlacementID, e.PublisherID, e.AccountID,
			e.Channel, e.DurationMs, int32(e.PercentVisible), e.AreaPx, b2u(e.IABViewable),
			int32(schemaVer(e.SchemaVersion)), bts(e.Timestamp),
		); err != nil {
			b.Abort()
			return fmt.Errorf("append view: %w", err)
		}
	}
	return b.Send()
}

func (c *ClickHouse) InsertAuctions(ctx context.Context, es []*AuctionEvent) error {
	if len(es) == 0 {
		return nil
	}
	b, err := c.conn.PrepareBatch(ctx, "INSERT INTO auctions")
	if err != nil {
		return fmt.Errorf("prepare auctions batch: %w", err)
	}
	for _, e := range es {
		if err := b.Append(
			e.TraceID, e.PlacementID, e.PublisherID, e.Channel, int32(e.NumBids), e.WinningBid,
			e.ClearingPrice, e.Currency, e.ClearingPriceUSD, e.WinnerDSP, e.DurationMs, e.DealID,
			int32(schemaVer(e.SchemaVersion)), bts(e.Timestamp),
		); err != nil {
			b.Abort()
			return fmt.Errorf("append auction: %w", err)
		}
	}
	return b.Send()
}

func (c *ClickHouse) InsertAuctionWins(ctx context.Context, es []*AuctionWinEvent) error {
	if len(es) == 0 {
		return nil
	}
	b, err := c.conn.PrepareBatch(ctx, `INSERT INTO auction_wins
		(trace_id, auction_id, winner_dsp, campaign_id, creative_id, placement_id,
		 publisher_id, advertiser_id, clearing_price, currency, bid_model, deal_id, channel,
		 schema_version, timestamp, segments)`)
	if err != nil {
		return fmt.Errorf("prepare auction_wins batch: %w", err)
	}
	for _, e := range es {
		if err := b.Append(
			e.TraceID, e.AuctionID, e.WinnerDSP, e.CampaignID, e.CreativeID, e.PlacementID,
			e.PublisherID, e.AdvertiserID, e.ClearingPrice, e.Currency, e.BidModel, e.DealID, e.Channel,
			int32(schemaVer(e.SchemaVersion)), bts(e.Timestamp), nonNilStrs(e.Segments),
		); err != nil {
			b.Abort()
			return fmt.Errorf("append auction_win: %w", err)
		}
	}
	return b.Send()
}

func (c *ClickHouse) InsertAuctionLosses(ctx context.Context, es []*AuctionLossEvent) error {
	if len(es) == 0 {
		return nil
	}
	b, err := c.conn.PrepareBatch(ctx, `INSERT INTO auction_losses
		(trace_id, auction_id, account_id, campaign_id, placement_id,
		 clearing_price_usd, loss_reason, channel, schema_version, timestamp)`)
	if err != nil {
		return fmt.Errorf("prepare auction_losses batch: %w", err)
	}
	for _, e := range es {
		if err := b.Append(
			e.TraceID, e.AuctionID, e.AccountID, e.CampaignID, e.PlacementID,
			e.ClearingPrice, e.LossReason, e.Channel, int32(schemaVer(e.SchemaVersion)), bts(e.Timestamp),
		); err != nil {
			b.Abort()
			return fmt.Errorf("append auction_loss: %w", err)
		}
	}
	return b.Send()
}

func (c *ClickHouse) InsertAuctionShades(ctx context.Context, es []*AuctionShadeEvent) error {
	if len(es) == 0 {
		return nil
	}
	b, err := c.conn.PrepareBatch(ctx, `INSERT INTO auction_shades
		(trace_id, account_id, campaign_id, placement_id,
		 savings_usd, clearing_price_usd, channel, schema_version, timestamp)`)
	if err != nil {
		return fmt.Errorf("prepare auction_shades batch: %w", err)
	}
	for _, e := range es {
		if err := b.Append(
			e.TraceID, e.AccountID, e.CampaignID, e.PlacementID,
			e.SavingsUSD, e.ClearingPrice, e.Channel, int32(schemaVer(e.SchemaVersion)), bts(e.Timestamp),
		); err != nil {
			b.Abort()
			return fmt.Errorf("append auction_shade: %w", err)
		}
	}
	return b.Send()
}

func (c *ClickHouse) InsertDSPCalls(ctx context.Context, es []*DSPCallEvent) error {
	if len(es) == 0 {
		return nil
	}
	// Explicit column list (not positional): no_bid_reason (Phase H) lands at the
	// END on ALTER-upgraded tables but MID-table on freshly CREATE'd ones, so a
	// bare "INSERT INTO dsp_calls" would map the wrong physical order. Naming the
	// columns makes the insert order-independent.
	b, err := c.conn.PrepareBatch(ctx, `INSERT INTO dsp_calls
		(trace_id, auction_id, channel, dsp_endpoint, bid_received, bid_price_usd,
		 latency_ms, timed_out, no_bid_reason, schema_version, timestamp)`)
	if err != nil {
		return fmt.Errorf("prepare dsp_calls batch: %w", err)
	}
	for _, e := range es {
		if err := b.Append(
			e.TraceID, e.AuctionID, e.Channel, e.DSPEndpoint, b2u(e.BidReceived),
			e.BidPriceUSD, e.LatencyMs, b2u(e.TimedOut), e.NoBidReason, int32(schemaVer(e.SchemaVersion)), bts(e.Timestamp),
		); err != nil {
			b.Abort()
			return fmt.Errorf("append dsp_call: %w", err)
		}
	}
	return b.Send()
}

func (c *ClickHouse) InsertMediaEvents(ctx context.Context, es []*MediaEvent) error {
	if len(es) == 0 {
		return nil
	}
	// Explicit column list (dsp_calls precedent): the attribution columns land
	// mid-table on fresh CREATEs but their physical position on ALTER-upgraded
	// tables is whatever the chained AFTERs produced — naming the columns makes
	// the insert order-independent either way.
	b, err := c.conn.PrepareBatch(ctx, `INSERT INTO media_events
		(trace_id, channel, event_type, position_ms, campaign_id, creative_id,
		 placement_id, publisher_id, account_id, error_code, schema_version, timestamp)`)
	if err != nil {
		return fmt.Errorf("prepare media_events batch: %w", err)
	}
	for _, e := range es {
		if e == nil {
			continue
		}
		if err := b.Append(
			e.TraceID, e.Channel, e.EventType, e.PositionMs,
			e.CampaignID, e.CreativeID, e.PlacementID, e.PublisherID, e.AccountID,
			e.ErrorCode, int32(1), bts(e.Timestamp),
		); err != nil {
			b.Abort()
			return fmt.Errorf("append media_event: %w", err)
		}
	}
	return b.Send()
}

// InsertBehaviourSignals bulk-inserts consent-gated behavioural observations
// (ADR 0006 phase 1). Append order matches the behaviour_signals CREATE TABLE
// column order in createTables (PrepareBatch binds positionally).
func (c *ClickHouse) InsertBehaviourSignals(ctx context.Context, es []*BehaviourSignalRow) error {
	if len(es) == 0 {
		return nil
	}
	b, err := c.conn.PrepareBatch(ctx, "INSERT INTO behaviour_signals")
	if err != nil {
		return fmt.Errorf("prepare behaviour_signals batch: %w", err)
	}
	for _, e := range es {
		if e == nil {
			continue
		}
		if err := b.Append(
			e.TraceID, e.Kind, e.UserID, e.HouseholdID, e.PlacementID, e.PublisherID,
			e.CampaignID, e.CreativeID, e.Channel, e.Categories, e.Geo, e.Device,
			e.AccountID, e.Tag, bts(e.ObservedAt),
		); err != nil {
			b.Abort()
			return fmt.Errorf("append behaviour_signal: %w", err)
		}
	}
	return b.Send()
}

// InsertProfileSignals bulk-inserts the EXPANDED per-id onboarding rows (ADR
// 0006 phase 1). One ProfileSignalRow per id (the reporting handler expands the
// batch upstream, mirroring the lake sink). Append order matches the
// profile_signals CREATE TABLE column order.
func (c *ClickHouse) InsertProfileSignals(ctx context.Context, es []*ProfileSignalRow) error {
	if len(es) == 0 {
		return nil
	}
	// Explicit column list (not positional): the ADR-0009 provider_id/data_party
	// columns land at the END on ALTER-upgraded tables but MID-table on freshly
	// CREATE'd ones, so a bare "INSERT INTO profile_signals" would map the wrong
	// physical order. Naming the columns makes the insert order-independent.
	b, err := c.conn.PrepareBatch(ctx, `INSERT INTO profile_signals
		(trace_id, ingest_trace_id, account_id, provider, provider_id, data_party, source, access,
		 segment_id, segment_name, visibility, consent, id_type, id_value, observed_at)`)
	if err != nil {
		return fmt.Errorf("prepare profile_signals batch: %w", err)
	}
	for _, e := range es {
		if e == nil {
			continue
		}
		if err := b.Append(
			e.TraceID, e.IngestTraceID, e.AccountID, e.Provider, e.ProviderID, e.DataParty, e.Source, e.Access,
			e.SegmentID, e.SegmentName, e.Visibility, b2u(e.Consent), e.IDType, e.IDValue, bts(e.ObservedAt),
		); err != nil {
			b.Abort()
			return fmt.Errorf("append profile_signal: %w", err)
		}
	}
	return b.Send()
}
