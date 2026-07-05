package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/targeting"
	"github.com/lib/pq"
)

// CampaignLoader reads every live line_item across all tenants and
// joins targeting + the primary creative into a flat models.Campaign.
//
// Used by the DSP's warm cache, not by request-time handlers. The query
// crosses tenants on purpose — the DSP holds the entire bid-eligible set
// in memory and filters by targeting rules per request. RLS is bypassed
// here by running outside any tenant context (no SET LOCAL); we read
// directly from the read replica.
//
// Filter precedence (first non-empty wins):
//  1. DSPID — preferred. Joins accounts.dsp_id; loads only campaigns from
//     advertisers the dsps row owns. Set by services aware of migration 022.
//  2. AccountIDs — legacy fallback. Direct allowlist of account UUIDs from
//     the YAML profile, used when DSPID is empty (boot before dsps row
//     exists, or fallback when the dsps lookup failed).
//  3. Neither — load all live campaigns (admin / cross-DSP analytics use).
type CampaignLoader struct {
	Store      *Store
	DSPID      string
	AccountIDs []string
}

// LoadAll returns the full bid-eligible campaign set.
//
// Status filter is intentionally permissive (live | paused) so admin tools
// can see paused campaigns; the bid handler is responsible for skipping
// non-live ones. Keeping the filter loose here means cache invalidation
// after a pause/resume doesn't need any extra logic on the read path.
func (l *CampaignLoader) LoadAll(ctx context.Context) ([]models.Campaign, error) {
	q := `
SELECT
    li.id::text,
    li.account_id::text,
    io.account_id::text AS advertiser_id,
    li.insertion_order_id::text,
    li.name,
    COALESCE(li.base_bid, 0)::float8,
    li.bid_currency,
    COALESCE(li.daily_budget, 0)::float8,
    COALESCE(io.budget, 0)::float8,
    li.bid_strategy,
    li.pacing_mode,
    li.status,
    COALESCE(tr.include_geo, '{}'),
    COALESCE(tr.exclude_geo, '{}'),
    COALESCE(tr.include_device, '{}'),
    COALESCE(tr.exclude_device, '{}'),
    COALESCE(tr.include_segments, '{}'),
    COALESCE(tr.exclude_segments, '{}'),
    COALESCE(tr.include_domains, '{}'),
    COALESCE(tr.exclude_domains, '{}'),
    COALESCE(tr.include_categories, '{}'),
    COALESCE(tr.exclude_categories, '{}'),
    COALESCE(tr.include_os, '{}'),
    COALESCE(tr.include_keywords, '{}'),
    COALESCE(tr.exclude_keywords, '{}'),
    COALESCE(tr.include_inventory_type, '{}'),
    COALESCE(tr.bid_modifiers::text, '{}'),
    COALESCE(cr.id::text, '') AS creative_id,
    -- Brand domain: prefer the explicit advertiser_domain column (set
    -- by seed); fall back to the host part of landing_url for rows
    -- that pre-date migration 028.
    COALESCE(NULLIF(cr.advertiser_domain, ''), SPLIT_PART(cr.landing_url, '/', 3), '') AS creative_domain,
    li.viewability_target_pct,
    COALESCE(cv.creatives_json, '[]')::text AS creatives_json
FROM line_items li
JOIN insertion_orders io ON io.id = li.insertion_order_id
JOIN accounts acc ON acc.id = li.account_id
LEFT JOIN targeting_rules tr ON tr.line_item_id = li.id
LEFT JOIN LATERAL (
    SELECT c.id, c.landing_url, c.advertiser_domain
    FROM line_item_creatives lic
    JOIN creatives c ON c.id = lic.creative_id
    WHERE lic.line_item_id = li.id
    ORDER BY lic.weight DESC, c.created_at ASC
    LIMIT 1
) cr ON true
LEFT JOIN LATERAL (
    SELECT jsonb_agg(jsonb_build_object(
        'id', c.id::text,
        'fmt', c.format,
        'w', c.width,
        'h', c.height,
        'dur', COALESCE(c.duration_seconds, 0),
        'media', COALESCE(c.asset_url, '')
    ) ORDER BY lic.weight DESC, c.created_at ASC) AS creatives_json
    FROM line_item_creatives lic
    JOIN creatives c ON c.id = lic.creative_id
    WHERE lic.line_item_id = li.id
) cv ON true
WHERE li.status IN ('live', 'paused')`

	var args []any
	switch {
	case l.DSPID != "":
		q += ` AND acc.dsp_id = $1::uuid`
		args = append(args, l.DSPID)
	case len(l.AccountIDs) > 0:
		q += ` AND li.account_id = ANY($1::uuid[])`
		args = append(args, pq.StringArray(l.AccountIDs))
	}

	rows, err := l.Store.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query campaigns: %w", err)
	}
	defer rows.Close()

	var out []models.Campaign
	for rows.Next() {
		var c models.Campaign
		var incGeo, excGeo, incDev, excDev, incSeg, excSeg, incDom, excDom, incCat, excCat pq.StringArray
		var incOS, incKw, excKw, incInv pq.StringArray
		var modifiersJSON string
		var viewTarget sql.NullInt32
		var creativesJSON string
		if err := rows.Scan(
			&c.ID, &c.AccountID, &c.AdvertiserID, &c.IOId, &c.Name,
			&c.BaseBid, &c.Currency, &c.DailyBudget, &c.TotalBudget,
			&c.BidModel, &c.PacingMode, &c.Status,
			&incGeo, &excGeo, &incDev, &excDev,
			&incSeg, &excSeg, &incDom, &excDom,
			&incCat, &excCat,
			&incOS, &incKw, &excKw, &incInv,
			&modifiersJSON,
			&c.CreativeID, &c.CreativeDomain,
			&viewTarget,
			&creativesJSON,
		); err != nil {
			return nil, fmt.Errorf("scan campaign: %w", err)
		}
		c.Creatives = parseCreativesJSON(creativesJSON)
		if viewTarget.Valid {
			pct := int(viewTarget.Int32)
			c.ViewabilityTargetPct = &pct
		}
		c.Targeting = targeting.Rules{
			Include: targeting.TargetingSet{
				Geo: incGeo, Device: incDev, Segments: incSeg,
				Domains: incDom, Categories: incCat,
				OS: incOS, Keywords: incKw, InventoryType: incInv,
			},
			Exclude: targeting.TargetingSet{
				Geo: excGeo, Device: excDev, Segments: excSeg,
				Domains: excDom, Categories: excCat,
				Keywords: excKw,
			},
		}
		c.Modifiers = parseModifiers(modifiersJSON)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// KeyOf satisfies warm.Loader[models.Campaign].
func (l *CampaignLoader) KeyOf(c models.Campaign) string { return c.ID }

// parseCreativesJSON decodes the jsonb_agg(...) output emitted by the
// CampaignLoader query into the runtime CampaignCreative slice. Tiny
// shape: [{id, w, h}, …] — ordered by line_item_creatives.weight DESC
// so the first element is also the legacy "primary" creative.
func parseCreativesJSON(raw string) []models.CampaignCreative {
	if raw == "" || raw == "[]" {
		return nil
	}
	var rows []struct {
		ID    string `json:"id"`
		Fmt   string `json:"fmt"`
		W     int    `json:"w"`
		H     int    `json:"h"`
		Dur   int    `json:"dur"`
		Media string `json:"media"`
	}
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		return nil
	}
	out := make([]models.CampaignCreative, 0, len(rows))
	for _, r := range rows {
		out = append(out, models.CampaignCreative{
			ID:       r.ID,
			Format:   r.Fmt,
			Width:    r.W,
			Height:   r.H,
			Duration: r.Dur,
			MediaURL: r.Media,
		})
	}
	return out
}

// parseModifiers turns the bid_modifiers JSONB column into the runtime
// targeting.Modifiers struct. Unknown keys are silently dropped — the column
// is intentionally schemaless to allow new dimensions without migrations.
func parseModifiers(raw string) targeting.Modifiers {
	if raw == "" || raw == "{}" {
		return targeting.Modifiers{}
	}
	var m struct {
		Device     map[string]float64 `json:"device"`
		GeoCountry map[string]float64 `json:"geo_country"`
	}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return targeting.Modifiers{}
	}
	return targeting.Modifiers{Device: m.Device, GeoCountry: m.GeoCountry}
}
