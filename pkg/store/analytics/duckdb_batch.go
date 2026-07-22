//go:build duckdb

package analytics

import (
	"context"
	"time"
)

// BatchInserter parity for DuckDB. Loops the existing single-row inserts —
// correct and idempotent-neutral. A future optimisation could wrap each in a
// single sql.Tx / appender for true bulk speed; parity is what the reporting
// batch consumer needs on this backend today.

var (
	_ BatchInserter = (*DuckDB)(nil)
	_ RollupReader  = (*DuckDB)(nil)
)

func (d *DuckDB) InsertImpressions(ctx context.Context, es []*ImpressionEvent) error {
	for _, e := range es {
		if err := d.InsertImpression(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (d *DuckDB) InsertClicks(ctx context.Context, es []*ClickEvent) error {
	for _, e := range es {
		if err := d.InsertClick(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (d *DuckDB) InsertConversions(ctx context.Context, es []*ConversionEvent) error {
	for _, e := range es {
		if err := d.InsertConversion(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (d *DuckDB) InsertViews(ctx context.Context, es []*ViewEvent) error {
	for _, e := range es {
		if err := d.InsertView(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (d *DuckDB) InsertAuctions(ctx context.Context, es []*AuctionEvent) error {
	for _, e := range es {
		if err := d.InsertAuction(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (d *DuckDB) InsertAuctionWins(ctx context.Context, es []*AuctionWinEvent) error {
	for _, e := range es {
		if err := d.InsertAuctionWin(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (d *DuckDB) InsertMediaEvents(ctx context.Context, es []*MediaEvent) error {
	for _, e := range es {
		if err := d.InsertMediaEvent(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (d *DuckDB) InsertDSPCalls(ctx context.Context, es []*DSPCallEvent) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, e := range es {
		ts := e.Timestamp
		if ts.IsZero() {
			ts = time.Now()
		}
		if _, err := d.db.ExecContext(ctx,
			`INSERT INTO dsp_calls (trace_id, auction_id, channel, dsp_endpoint, bid_received,
				bid_price_usd, latency_ms, timed_out, schema_version, timestamp)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			e.TraceID, e.AuctionID, e.Channel, e.DSPEndpoint, e.BidReceived,
			e.BidPriceUSD, e.LatencyMs, e.TimedOut, schemaVer(e.SchemaVersion), ts); err != nil {
			return err
		}
	}
	return nil
}

// InsertBehaviourSignals bulk-inserts consent-gated behavioural rows (ADR 0006
// phase 1) — DuckDB parity for the ClickHouse bulk insert.
func (d *DuckDB) InsertBehaviourSignals(ctx context.Context, es []*BehaviourSignalRow) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, e := range es {
		if e == nil {
			continue
		}
		at := e.ObservedAt
		if at.IsZero() {
			at = time.Now()
		}
		if _, err := d.db.ExecContext(ctx,
			`INSERT INTO behaviour_signals (trace_id, kind, user_id, household_id, placement_id,
				publisher_id, campaign_id, creative_id, channel, categories, geo, device,
				account_id, tag, observed_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			e.TraceID, e.Kind, e.UserID, e.HouseholdID, e.PlacementID, e.PublisherID,
			e.CampaignID, e.CreativeID, e.Channel, e.Categories, e.Geo, e.Device,
			e.AccountID, e.Tag, at); err != nil {
			return err
		}
	}
	return nil
}

// InsertProfileSignals bulk-inserts the EXPANDED per-id onboarding rows (ADR
// 0006 phase 1) — DuckDB parity for the ClickHouse bulk insert.
func (d *DuckDB) InsertProfileSignals(ctx context.Context, es []*ProfileSignalRow) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, e := range es {
		if e == nil {
			continue
		}
		at := e.ObservedAt
		if at.IsZero() {
			at = time.Now()
		}
		if _, err := d.db.ExecContext(ctx,
			`INSERT INTO profile_signals (trace_id, account_id, provider, source, access,
				segment_id, segment_name, visibility, consent, id_type, id_value, observed_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			e.TraceID, e.AccountID, e.Provider, e.Source, e.Access, e.SegmentID,
			e.SegmentName, e.Visibility, e.Consent, e.IDType, e.IDValue, at); err != nil {
			return err
		}
	}
	return nil
}
