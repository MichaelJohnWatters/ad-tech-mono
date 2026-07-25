package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/publisheradserver"
	"github.com/lib/pq"
)

// PublisherLineItemLoader reads every active publisher-side line item into
// the publisher-adserver's warm cache. Used for arbitration on the hot
// path. Pattern mirrors DealLoader.
type PublisherLineItemLoader struct {
	Store *Store
}

func (l *PublisherLineItemLoader) LoadAll(ctx context.Context) ([]publisheradserver.PublisherLineItem, error) {
	const q = `
SELECT
    pli.id::text,
    pli.account_id::text,
    pli.publisher_id::text,
    pli.name,
    pli.demand_source,
    pli.priority_tier,
    COALESCE(pli.placement_ids::text[], '{}'),
    pli.impressions_committed,
    pli.delivery_start,
    pli.delivery_end,
    COALESCE(pli.cpm, 0)::float8,
    pli.currency,
    pli.pacing_mode,
    pli.status,
    COALESCE(
        (SELECT array_agg(plic.creative_id::text)
         FROM publisher_line_item_creatives plic
         WHERE plic.publisher_line_item_id = pli.id),
        '{}'
    ) AS creative_ids
FROM publisher_line_items pli
WHERE pli.status = 'active'`

	rows, closeRows, err := l.Store.QueryPlatform(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query publisher_line_items: %w", err)
	}
	defer closeRows()

	var out []publisheradserver.PublisherLineItem
	for rows.Next() {
		var li publisheradserver.PublisherLineItem
		var placementIDs, creativeIDs pq.StringArray
		var deliveryStart, deliveryEnd sql.NullTime
		if err := rows.Scan(
			&li.ID, &li.AccountID, &li.PublisherID,
			&li.Name, &li.DemandSource, &li.PriorityTier,
			&placementIDs, &li.ImpressionsCommitted,
			&deliveryStart, &deliveryEnd,
			&li.CPM, &li.Currency, &li.PacingMode, &li.Status,
			&creativeIDs,
		); err != nil {
			return nil, fmt.Errorf("scan publisher_line_item: %w", err)
		}
		li.PlacementIDs = placementIDs
		li.CreativeIDs = creativeIDs
		if deliveryStart.Valid {
			t := deliveryStart.Time
			li.DeliveryStart = &t
		}
		if deliveryEnd.Valid {
			t := deliveryEnd.Time
			li.DeliveryEnd = &t
		}
		out = append(out, li)
	}
	return out, rows.Err()
}

func (l *PublisherLineItemLoader) KeyOf(li publisheradserver.PublisherLineItem) string {
	return li.ID
}
