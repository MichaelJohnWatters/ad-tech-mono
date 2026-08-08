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
		if limit, window, scope, ok := parseLineItemCap(capsJSON); ok {
			out = append(out, models.FreqCapRule{CampaignID: campaignID, Limit: limit, Window: window, Scope: scope})
		}
	}
	return out, rows.Err()
}

// KeyOf satisfies warm.Loader[models.FreqCapRule].
func (l *FreqCapLoader) KeyOf(r models.FreqCapRule) string { return r.CampaignID }

// ParseLineItemCap is THE decoder of the frequency_caps line_item entry —
// writer: cmd/dsp freqCapInput.validateAndJSON; readers: the FreqCapLoader
// below (enforcement) and the DSP's edit-form prefill. One exported symbol so
// the two read sides can't drift from each other or the writer. Returns the
// raw window enum ("hour"/"day"/"week"); ok is false when there is no
// line_item entry with a positive limit. Scope rides as an extra field ON the
// line_item entry (not a separate dimension) so pre-scope decoders keep
// resolving the cap; any value other than "household" — including absent, the
// pre-scope shape — normalises to the per-user default.
func ParseLineItemCap(raw string) (limit int, window, scope string, ok bool) {
	if raw == "" || raw == "[]" {
		return 0, "", "", false
	}
	var caps []struct {
		Dimension string `json:"dimension"`
		Window    string `json:"window"`
		Limit     int    `json:"limit"`
		Scope     string `json:"scope"`
	}
	if err := json.Unmarshal([]byte(raw), &caps); err != nil {
		return 0, "", "", false
	}
	for _, c := range caps {
		if c.Dimension == "line_item" && c.Limit > 0 {
			scope = models.FreqCapScopeUser
			if c.Scope == models.FreqCapScopeHousehold {
				scope = models.FreqCapScopeHousehold
			}
			return c.Limit, c.Window, scope, true
		}
	}
	return 0, "", "", false
}

// parseLineItemCap is the loader-local form: window mapped to its TTL.
func parseLineItemCap(raw string) (limit int, window time.Duration, scope string, ok bool) {
	l, w, s, ok := ParseLineItemCap(raw)
	if !ok {
		return 0, 0, "", false
	}
	return l, windowDuration(w), s, true
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
