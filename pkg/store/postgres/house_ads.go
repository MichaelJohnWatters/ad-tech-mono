package postgres

import (
	"context"
	"fmt"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/houseads"
)

// HouseAdLoader reads every house ad into the publisher-adserver's warm cache,
// so a no-bid can be filled from an ops-configured fallback creative without a
// DB round-trip on the serve path. House ads are platform-global (no account
// scope) — see migration 053. Pattern mirrors PlacementLoader.
type HouseAdLoader struct {
	Store *Store
}

func (l *HouseAdLoader) LoadAll(ctx context.Context) ([]houseads.HouseAd, error) {
	const q = `
SELECT id::text, format, name, markup, COALESCE(landing_url, ''), enabled, weight, created_at, updated_at
FROM house_ads`
	rows, err := l.Store.read.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query house_ads: %w", err)
	}
	defer rows.Close()
	var out []houseads.HouseAd
	for rows.Next() {
		var h houseads.HouseAd
		if err := rows.Scan(&h.ID, &h.Format, &h.Name, &h.Markup, &h.LandingURL,
			&h.Enabled, &h.Weight, &h.CreatedAt, &h.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan house_ad: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (l *HouseAdLoader) KeyOf(h houseads.HouseAd) string { return h.ID }
