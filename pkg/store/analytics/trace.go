package analytics

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// This file implements TraceReader (see analytics.go) on both backends: the
// MemoryStore (tests + memory backend) and ClickHouse (prod). Both authorize a
// scoped read by requiring the trace to have an impression matching the scope,
// then return every event on the trace ordered by time.

func defaultLimit(limit int) int {
	if limit <= 0 {
		return 50
	}
	if limit > 500 {
		return 500
	}
	return limit
}

// ---- MemoryStore ----

func (s *MemoryStore) EventsByTrace(_ context.Context, traceID string, scope TraceScope) ([]TraceEvent, error) {
	if traceID == "" {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	if !scope.Unscoped() {
		owned := false
		for i := range s.impressions {
			e := &s.impressions[i]
			if e.TraceID == traceID && matchesScope(e.AccountID, e.PublisherID, scope) {
				owned = true
				break
			}
		}
		if !owned {
			return nil, nil
		}
	}

	var out []TraceEvent
	for i := range s.auctionWins {
		if w := &s.auctionWins[i]; w.TraceID == traceID {
			out = append(out, TraceEvent{
				Kind: "auction_win", Timestamp: w.Timestamp, WinnerDSP: w.WinnerDSP,
				CampaignID: w.CampaignID, CreativeID: w.CreativeID, PlacementID: w.PlacementID,
				PublisherID: w.PublisherID, AdvertiserID: w.AdvertiserID, ClearingPriceUSD: w.ClearingPrice,
				BidModel: w.BidModel, DealID: w.DealID, Segments: w.Segments,
			})
		}
	}
	for i := range s.impressions {
		if e := &s.impressions[i]; e.TraceID == traceID {
			out = append(out, TraceEvent{
				Kind: "impression", Timestamp: e.Timestamp, CampaignID: e.CampaignID,
				CreativeID: e.CreativeID, PlacementID: e.PlacementID, PublisherID: e.PublisherID,
				AccountID: e.AccountID, BidModel: e.BidModel, ClearingPriceUSD: e.ClearingPriceUSD, DealID: e.DealID,
			})
		}
	}
	for i := range s.views {
		if e := &s.views[i]; e.TraceID == traceID {
			et := "not-viewable"
			if e.IABViewable {
				et = "viewable"
			}
			out = append(out, TraceEvent{
				Kind: "view", Timestamp: e.Timestamp, CampaignID: e.CampaignID,
				PlacementID: e.PlacementID, PublisherID: e.PublisherID, EventType: et,
			})
		}
	}
	for i := range s.clicks {
		if e := &s.clicks[i]; e.TraceID == traceID {
			out = append(out, TraceEvent{
				Kind: "click", Timestamp: e.Timestamp, CampaignID: e.CampaignID,
				CreativeID: e.CreativeID, PlacementID: e.PlacementID, PublisherID: e.PublisherID,
			})
		}
	}
	for i := range s.conversions {
		if e := &s.conversions[i]; e.TraceID == traceID {
			out = append(out, TraceEvent{
				Kind: "conversion", Timestamp: e.Timestamp, CampaignID: e.CampaignID,
				PlacementID: e.PlacementID, EventType: e.ConversionType,
			})
		}
	}
	for i := range s.mediaEvents {
		if e := &s.mediaEvents[i]; e.TraceID == traceID {
			out = append(out, TraceEvent{Kind: "media", Timestamp: e.Timestamp, EventType: e.EventType})
		}
	}
	sortByTime(out)
	return out, nil
}

func (s *MemoryStore) RecentImpressions(_ context.Context, scope TraceScope, limit int) ([]ImpressionRow, error) {
	limit = defaultLimit(limit)
	s.mu.RLock()
	defer s.mu.RUnlock()
	var rows []ImpressionRow
	for i := range s.impressions {
		e := &s.impressions[i]
		if !scope.Unscoped() && !matchesScope(e.AccountID, e.PublisherID, scope) {
			continue
		}
		rows = append(rows, ImpressionRow{
			TraceID: e.TraceID, Timestamp: e.Timestamp, CampaignID: e.CampaignID,
			CreativeID: e.CreativeID, PlacementID: e.PlacementID, PublisherID: e.PublisherID,
			BidModel: e.BidModel, ClearingPriceUSD: e.ClearingPriceUSD, DealID: e.DealID,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Timestamp.After(rows[j].Timestamp) })
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

func matchesScope(accountID, publisherID string, scope TraceScope) bool {
	if scope.AccountID != "" && accountID == scope.AccountID {
		return true
	}
	if scope.PublisherID != "" && publisherID == scope.PublisherID {
		return true
	}
	return false
}

func sortByTime(evs []TraceEvent) {
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].Timestamp.Before(evs[j].Timestamp) })
}

// ---- ClickHouse ----

// traceScopeWhere builds the "owns this trace" predicate on the impressions
// table (which carries BOTH account_id and publisher_id). Returns the SQL
// fragment + its bind arg, or ("", nil) when unscoped.
func traceScopeWhere(scope TraceScope) (string, string) {
	if scope.AccountID != "" {
		return "account_id = ?", scope.AccountID
	}
	if scope.PublisherID != "" {
		return "publisher_id = ?", scope.PublisherID
	}
	return "", ""
}

func (c *ClickHouse) traceOwned(ctx context.Context, traceID string, scope TraceScope) (bool, error) {
	where, arg := traceScopeWhere(scope)
	if where == "" {
		return true, nil
	}
	var n uint64
	if err := c.db.QueryRowContext(ctx, "SELECT count() FROM impressions WHERE trace_id = ? AND "+where, traceID, arg).Scan(&n); err != nil {
		return false, fmt.Errorf("trace owned: %w", err)
	}
	return n > 0, nil
}

func (c *ClickHouse) EventsByTrace(ctx context.Context, traceID string, scope TraceScope) ([]TraceEvent, error) {
	if traceID == "" {
		return nil, nil
	}
	if !scope.Unscoped() {
		owned, err := c.traceOwned(ctx, traceID, scope)
		if err != nil {
			return nil, err
		}
		if !owned {
			return nil, nil
		}
	}

	var out []TraceEvent
	scan := func(kind, query string, fn func(*TraceEvent) []any) error {
		rows, err := c.db.QueryContext(ctx, query, traceID)
		if err != nil {
			return fmt.Errorf("%s: %w", kind, err)
		}
		defer rows.Close()
		for rows.Next() {
			e := TraceEvent{Kind: kind}
			if err := rows.Scan(fn(&e)...); err != nil {
				return fmt.Errorf("scan %s: %w", kind, err)
			}
			out = append(out, e)
		}
		return rows.Err()
	}

	if err := scan("auction_win",
		`SELECT winner_dsp, campaign_id, creative_id, placement_id, publisher_id, advertiser_id, clearing_price, bid_model, deal_id, segments, timestamp FROM auction_wins WHERE trace_id = ?`,
		func(e *TraceEvent) []any {
			return []any{&e.WinnerDSP, &e.CampaignID, &e.CreativeID, &e.PlacementID, &e.PublisherID, &e.AdvertiserID, &e.ClearingPriceUSD, &e.BidModel, &e.DealID, &e.Segments, &e.Timestamp}
		}); err != nil {
		return nil, err
	}
	if err := scan("impression",
		`SELECT campaign_id, creative_id, placement_id, publisher_id, account_id, bid_model, clearing_price_usd, deal_id, timestamp FROM impressions WHERE trace_id = ?`,
		func(e *TraceEvent) []any {
			return []any{&e.CampaignID, &e.CreativeID, &e.PlacementID, &e.PublisherID, &e.AccountID, &e.BidModel, &e.ClearingPriceUSD, &e.DealID, &e.Timestamp}
		}); err != nil {
		return nil, err
	}
	// Views need a post-scan conversion (iab_viewable bool → event_type label),
	// so they don't use the generic scan helper.
	if err := func() error {
		rows, err := c.db.QueryContext(ctx, `SELECT campaign_id, placement_id, publisher_id, iab_viewable, timestamp FROM views WHERE trace_id = ?`, traceID)
		if err != nil {
			return fmt.Errorf("view: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			e := TraceEvent{Kind: "view"}
			var viewable uint8
			if err := rows.Scan(&e.CampaignID, &e.PlacementID, &e.PublisherID, &viewable, &e.Timestamp); err != nil {
				return fmt.Errorf("scan view: %w", err)
			}
			if viewable == 1 {
				e.EventType = "viewable"
			} else {
				e.EventType = "not-viewable"
			}
			out = append(out, e)
		}
		return rows.Err()
	}(); err != nil {
		return nil, err
	}
	if err := scan("click",
		`SELECT campaign_id, creative_id, placement_id, publisher_id, timestamp FROM clicks WHERE trace_id = ?`,
		func(e *TraceEvent) []any {
			return []any{&e.CampaignID, &e.CreativeID, &e.PlacementID, &e.PublisherID, &e.Timestamp}
		}); err != nil {
		return nil, err
	}
	if err := scan("conversion",
		`SELECT campaign_id, placement_id, conversion_type, timestamp FROM conversions WHERE trace_id = ?`,
		func(e *TraceEvent) []any {
			return []any{&e.CampaignID, &e.PlacementID, &e.EventType, &e.Timestamp}
		}); err != nil {
		return nil, err
	}
	if err := scan("media",
		`SELECT event_type, timestamp FROM media_events WHERE trace_id = ?`,
		func(e *TraceEvent) []any {
			return []any{&e.EventType, &e.Timestamp}
		}); err != nil {
		return nil, err
	}
	// DSP-level enforcement blocks (Phase H): only rows carrying a reason — a DSP
	// that declined for a stated cause (e.g. adcert_invalid). Ordinary no-bids
	// (empty reason) are intentionally NOT surfaced, to keep the timeline to the
	// signal: "this DSP was blocked, and why".
	if err := scan("dsp_block",
		`SELECT dsp_endpoint, no_bid_reason, timestamp FROM dsp_calls WHERE trace_id = ? AND no_bid_reason != ''`,
		func(e *TraceEvent) []any {
			return []any{&e.DSPEndpoint, &e.NoBidReason, &e.Timestamp}
		}); err != nil {
		return nil, err
	}

	sortByTime(out)
	return out, nil
}

func (c *ClickHouse) RecentImpressions(ctx context.Context, scope TraceScope, limit int) ([]ImpressionRow, error) {
	limit = defaultLimit(limit)
	q := `SELECT trace_id, campaign_id, creative_id, placement_id, publisher_id, bid_model, clearing_price_usd, deal_id, timestamp FROM impressions`
	args := []any{}
	if where, arg := traceScopeWhere(scope); where != "" {
		q += " WHERE " + where
		args = append(args, arg)
	}
	q += " ORDER BY timestamp DESC LIMIT ?"
	args = append(args, limit)
	rows, err := c.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("recent impressions: %w", err)
	}
	defer rows.Close()
	var out []ImpressionRow
	for rows.Next() {
		var r ImpressionRow
		var ts time.Time
		if err := rows.Scan(&r.TraceID, &r.CampaignID, &r.CreativeID, &r.PlacementID, &r.PublisherID, &r.BidModel, &r.ClearingPriceUSD, &r.DealID, &ts); err != nil {
			return nil, fmt.Errorf("scan impression: %w", err)
		}
		r.Timestamp = ts
		out = append(out, r)
	}
	return out, rows.Err()
}
