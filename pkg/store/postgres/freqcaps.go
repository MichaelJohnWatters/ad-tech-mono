package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
)

// FreqCapLoader reads per-campaign frequency caps from the line_item-dimension
// entry of targeting_rules.frequency_caps, for the ad server's warm cache.
//
// Only campaigns that actually declare a line_item cap (limit > 0) are
// emitted — the ad server falls back to the platform-default cap for any
// campaign not in the cache. Crosses tenants on purpose (the ad server holds
// the whole set in memory), same as the CampaignLoader.
type FreqCapLoader struct {
	Store *Store
}

// LoadAll returns one FreqCapRule per campaign that declares a line_item cap.
func (l *FreqCapLoader) LoadAll(ctx context.Context) ([]models.FreqCapRule, error) {
	const q = `
SELECT li.id::text, COALESCE(tr.frequency_caps::text, '[]')
FROM line_items li
JOIN targeting_rules tr ON tr.line_item_id = li.id
WHERE li.status IN ('live', 'paused')`

	rows, closeRows, err := l.Store.QueryPlatform(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query freq caps: %w", err)
	}
	defer closeRows()

	var out []models.FreqCapRule
	for rows.Next() {
		var campaignID, capsJSON string
		if err := rows.Scan(&campaignID, &capsJSON); err != nil {
			return nil, fmt.Errorf("scan freq cap: %w", err)
		}
		if limit, window, ok := parseLineItemCap(capsJSON); ok {
			out = append(out, models.FreqCapRule{CampaignID: campaignID, Limit: limit, Window: window})
		}
	}
	return out, rows.Err()
}

// KeyOf satisfies warm.Loader[models.FreqCapRule].
func (l *FreqCapLoader) KeyOf(r models.FreqCapRule) string { return r.CampaignID }

// parseLineItemCap decodes the frequency_caps JSONB array and returns the
// line_item-dimension cap (limit + window duration). ok is false when there is
// no line_item entry with a positive limit.
func parseLineItemCap(raw string) (limit int, window time.Duration, ok bool) {
	if raw == "" || raw == "[]" {
		return 0, 0, false
	}
	var caps []struct {
		Dimension string `json:"dimension"`
		Window    string `json:"window"`
		Limit     int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(raw), &caps); err != nil {
		return 0, 0, false
	}
	for _, c := range caps {
		if c.Dimension == "line_item" && c.Limit > 0 {
			return c.Limit, windowDuration(c.Window), true
		}
	}
	return 0, 0, false
}

// windowDuration maps the frequency_caps "window" enum to a TTL. Unknown or
// empty values default to a day, matching the column's documented shape.
func windowDuration(window string) time.Duration {
	switch window {
	case "hour":
		return time.Hour
	case "week":
		return 7 * 24 * time.Hour
	case "day", "":
		return 24 * time.Hour
	default:
		return 24 * time.Hour
	}
}
