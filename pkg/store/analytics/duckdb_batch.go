//go:build duckdb

package analytics

import "context"

// BatchInserter parity for DuckDB. Loops the existing single-row inserts —
// correct and idempotent-neutral. A future optimisation could wrap each in a
// single sql.Tx / appender for true bulk speed; parity is what the reporting
// batch consumer needs on this backend today.

var _ BatchInserter = (*DuckDB)(nil)

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
