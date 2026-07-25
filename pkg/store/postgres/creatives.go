package postgres

import (
	"context"
	"fmt"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
)

// CreativeLoader reads creative metadata for the ad server's warm cache.
//
// HTML bodies live in Minio (the ad server fetches them lazily and L1-caches
// them); this loader only returns the small fixed-size metadata: dimensions,
// landing URL, review status, format. Approved-only — paused/rejected
// creatives never reach the ad serving path.
type CreativeLoader struct {
	Store *Store
}

func (l *CreativeLoader) LoadAll(ctx context.Context) ([]models.Creative, error) {
	const q = `
SELECT
    id::text,
    account_id::text,
    name,
    format,
    COALESCE(width, 0),
    COALESCE(height, 0),
    COALESCE(asset_url, ''),
    COALESCE(html_content, ''),
    landing_url,
    COALESCE(duration_seconds, 0),
    review_status,
    COALESCE(rejection_reason, ''),
    created_at,
    updated_at
FROM creatives
WHERE review_status = 'approved'`

	rows, closeRows, err := l.Store.QueryPlatform(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query creatives: %w", err)
	}
	defer closeRows()

	var out []models.Creative
	for rows.Next() {
		var c models.Creative
		if err := rows.Scan(
			&c.ID, &c.AccountID, &c.Name, &c.Format,
			&c.Width, &c.Height, &c.AssetURL, &c.HTMLContent,
			&c.LandingURL, &c.DurationSeconds,
			&c.ReviewStatus, &c.RejectionReason,
			&c.CreatedAt, &c.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan creative: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (l *CreativeLoader) KeyOf(c models.Creative) string { return c.ID }
