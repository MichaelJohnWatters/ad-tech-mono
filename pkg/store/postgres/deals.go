package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
	"github.com/lib/pq"
)

// DealLoader reads every active deal (PG, Preferred, PMP, Open) into the
// exchange's warm cache. Matching logic lives in pkg/deals (when built);
// this just supplies the rows.
type DealLoader struct {
	Store *Store
}

func (l *DealLoader) LoadAll(ctx context.Context) ([]models.Deal, error) {
	const q = `
SELECT
    id::text,
    publisher_id::text,
    account_id::text,
    name,
    deal_type,
    COALESCE(price, 0)::float8,
    COALESCE(price_currency, 'USD'),
    COALESCE(advertiser_ids::text[], '{}'),
    COALESCE(placement_ids::text[], '{}'),
    COALESCE(guaranteed_volume, 0),
    start_date,
    end_date,
    status,
    viewability_target_pct
FROM deals
WHERE status = 'active'`

	rows, closeRows, err := l.Store.QueryPlatform(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query deals: %w", err)
	}
	defer closeRows()

	var out []models.Deal
	for rows.Next() {
		var d models.Deal
		var advIDs, plIDs pq.StringArray
		var viewTarget sql.NullInt32
		if err := rows.Scan(
			&d.ID, &d.PublisherID, &d.AccountID, &d.Name,
			&d.DealType, &d.Price, &d.PriceCurrency,
			&advIDs, &plIDs, &d.GuaranteedVolume,
			&d.StartDate, &d.EndDate, &d.Status,
			&viewTarget,
		); err != nil {
			return nil, fmt.Errorf("scan deal: %w", err)
		}
		d.AdvertiserIDs = advIDs
		d.PlacementIDs = plIDs
		if viewTarget.Valid {
			pct := int(viewTarget.Int32)
			d.ViewabilityTargetPct = &pct
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (l *DealLoader) KeyOf(d models.Deal) string { return d.ID }
