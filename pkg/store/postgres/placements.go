package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
)

// PlacementRow is the warm-cache view used by the SSP — placement fields
// joined with the parent publisher's domain so the bid handler doesn't have
// to do a second lookup. Held in memory across requests; no transaction needed.
type PlacementRow struct {
	models.Placement
	PublisherDomain string
	PublisherName   string
	Categories      []string
}

// PlacementLoader reads every active placement joined with its publisher.
//
// SSPs at scale would shard by publisher account, but in local dev one SSP
// pod serves all publishers — the join + denormalization keeps lookups
// O(1) at request time.
type PlacementLoader struct {
	Store *Store
}

func (l *PlacementLoader) LoadAll(ctx context.Context) ([]PlacementRow, error) {
	const q = `
SELECT
    pl.id::text,
    pl.publisher_id::text,
    pl.account_id::text,
    pl.name,
    pl.format,
    COALESCE(pl.width, 0),
    COALESCE(pl.height, 0),
    pl.floor_price::float8,
    pl.floor_currency,
    COALESCE(pl.page_url_pattern, ''),
    pl.status,
    COALESCE(pl.floor_config::text, '{}'),
    COALESCE(pl.video_config::text, '{}'),
    pl.created_at,
    pl.updated_at,
    pub.name,
    pub.domain
FROM placements pl
JOIN publishers pub ON pub.id = pl.publisher_id
WHERE pl.status = 'active'`

	rows, err := l.Store.read.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query placements: %w", err)
	}
	defer rows.Close()

	var out []PlacementRow
	for rows.Next() {
		var r PlacementRow
		var floorJSON, videoJSON string
		if err := rows.Scan(
			&r.ID, &r.PublisherID, &r.AccountID, &r.Name, &r.Format,
			&r.Width, &r.Height, &r.FloorPrice, &r.FloorCurrency,
			&r.PageURLPattern, &r.Status, &floorJSON, &videoJSON,
			&r.CreatedAt, &r.UpdatedAt,
			&r.PublisherName, &r.PublisherDomain,
		); err != nil {
			return nil, fmt.Errorf("scan placement: %w", err)
		}
		r.FloorConfig = parseFloorConfig(floorJSON)
		r.VideoConfig = parseFloorConfig(videoJSON) // same permissive JSONB→map parse
		r.Categories = extractCategories(r.FloorConfig)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (l *PlacementLoader) KeyOf(r PlacementRow) string { return r.ID }

func parseFloorConfig(raw string) map[string]any {
	if raw == "" || raw == "{}" {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return map[string]any{}
	}
	return m
}

// extractCategories pulls the categories array out of floor_config's
// freeform JSONB. The seed nests it there to avoid a separate column.
func extractCategories(m map[string]any) []string {
	raw, ok := m["categories"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
