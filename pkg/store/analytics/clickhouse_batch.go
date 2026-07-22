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
			int32(schemaVer(e.SchemaVersion)), bts(e.Timestamp),
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
	b, err := c.conn.PrepareBatch(ctx, "INSERT INTO conversions")
	if err != nil {
		return fmt.Errorf("prepare conversions batch: %w", err)
	}
	for _, e := range es {
		if err := b.Append(
			e.TraceID, e.CampaignID, e.CreativeID, e.PlacementID, e.AccountID, e.ConversionType,
			e.Revenue, e.Currency, e.RevenueUSD, int32(schemaVer(e.SchemaVersion)), bts(e.Timestamp),
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
	b, err := c.conn.PrepareBatch(ctx, "INSERT INTO views")
	if err != nil {
		return fmt.Errorf("prepare views batch: %w", err)
	}
	for _, e := range es {
		if err := b.Append(
			e.TraceID, e.CampaignID, e.CreativeID, e.PlacementID, e.PublisherID, e.AccountID,
			e.DurationMs, int32(e.PercentVisible), e.AreaPx, b2u(e.IABViewable),
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
	b, err := c.conn.PrepareBatch(ctx, "INSERT INTO auction_wins")
	if err != nil {
		return fmt.Errorf("prepare auction_wins batch: %w", err)
	}
	for _, e := range es {
		if err := b.Append(
			e.TraceID, e.AuctionID, e.WinnerDSP, e.CampaignID, e.CreativeID, e.PlacementID,
			e.PublisherID, e.AdvertiserID, e.ClearingPrice, e.Currency, e.BidModel, e.DealID, e.Channel,
			int32(schemaVer(e.SchemaVersion)), bts(e.Timestamp),
		); err != nil {
			b.Abort()
			return fmt.Errorf("append auction_win: %w", err)
		}
	}
	return b.Send()
}

func (c *ClickHouse) InsertDSPCalls(ctx context.Context, es []*DSPCallEvent) error {
	if len(es) == 0 {
		return nil
	}
	b, err := c.conn.PrepareBatch(ctx, "INSERT INTO dsp_calls")
	if err != nil {
		return fmt.Errorf("prepare dsp_calls batch: %w", err)
	}
	for _, e := range es {
		if err := b.Append(
			e.TraceID, e.AuctionID, e.Channel, e.DSPEndpoint, b2u(e.BidReceived),
			e.BidPriceUSD, e.LatencyMs, b2u(e.TimedOut), int32(schemaVer(e.SchemaVersion)), bts(e.Timestamp),
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
	b, err := c.conn.PrepareBatch(ctx, "INSERT INTO media_events")
	if err != nil {
		return fmt.Errorf("prepare media_events batch: %w", err)
	}
	for _, e := range es {
		if e == nil {
			continue
		}
		if err := b.Append(
			e.TraceID, e.Channel, e.EventType, e.PositionMs, int32(1), bts(e.Timestamp),
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
	b, err := c.conn.PrepareBatch(ctx, "INSERT INTO profile_signals")
	if err != nil {
		return fmt.Errorf("prepare profile_signals batch: %w", err)
	}
	for _, e := range es {
		if e == nil {
			continue
		}
		if err := b.Append(
			e.TraceID, e.AccountID, e.Provider, e.Source, e.Access, e.SegmentID,
			e.SegmentName, e.Visibility, b2u(e.Consent), e.IDType, e.IDValue, bts(e.ObservedAt),
		); err != nil {
			b.Abort()
			return fmt.Errorf("append profile_signal: %w", err)
		}
	}
	return b.Send()
}
