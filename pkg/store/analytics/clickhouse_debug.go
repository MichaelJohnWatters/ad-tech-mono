package analytics

import (
	"context"
	"strconv"
	"time"
)

// DebugReader parity for ClickHouse. These back the reporting /debug
// read-back endpoints (e2e assertions, ops "did this event land?" checks) so
// the clickhouse backend behaves like memory instead of returning 501.
//
// All are simple filtered SELECTs on the raw tables. On any query error they
// return the zero value + log — a debug read-back is never worth failing a
// request over. Note ClickHouse insert visibility is near-immediate but not
// synchronous; callers (the e2e harness) already poll / RefreshAllCaches.

func (c *ClickHouse) count(query string, args ...any) int {
	var n uint64
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		c.log.Error("clickhouse: debug count query failed", "error", err)
		return 0
	}
	return int(n)
}

func (c *ClickHouse) AuctionWinCount(traceID string) int {
	return c.count(`SELECT count() FROM auction_wins WHERE trace_id = ?`, traceID)
}

func (c *ClickHouse) AuctionWinByBidModel(traceID, bidModel string) int {
	return c.count(`SELECT count() FROM auction_wins WHERE trace_id = ? AND bid_model = ?`, traceID, bidModel)
}

func (c *ClickHouse) BudgetDepletionsByCampaign(campaignID string) int {
	return c.count(`SELECT count() FROM budget_depletions WHERE campaign_id = ?`, campaignID)
}

func (c *ClickHouse) TrackerRejectionsByReason(reason string) int {
	return c.count(`SELECT count() FROM tracker_rejections WHERE reason = ?`, reason)
}

func (c *ClickHouse) ServeNoFillsByTrace(traceID string) int {
	return c.count(`SELECT count() FROM serve_no_fills WHERE trace_id = ?`, traceID)
}

func (c *ClickHouse) MediaEventsByTrace(traceID, channel, eventType, errorCode string) int {
	q := `SELECT count() FROM media_events WHERE trace_id = ?`
	args := []any{traceID}
	if channel != "" {
		q += ` AND channel = ?`
		args = append(args, channel)
	}
	if eventType != "" {
		q += ` AND event_type = ?`
		args = append(args, eventType)
	}
	// Numeric string ("405") so "0" explicitly filters no-code rows;
	// empty = no filter. Parsed here rather than bound raw so a junk
	// value can't type-error the ClickHouse query.
	if errorCode != "" {
		if code, err := strconv.Atoi(errorCode); err == nil {
			q += ` AND error_code = ?`
			args = append(args, int32(code))
		}
	}
	return c.count(q, args...)
}

func (c *ClickHouse) CampaignStateChangesByCampaign(campaignID string) []CampaignStateChange {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := c.db.QueryContext(ctx,
		`SELECT campaign_id, account_id, old_state, new_state, reason, timestamp
		 FROM campaign_state_changes WHERE campaign_id = ? ORDER BY timestamp`, campaignID)
	if err != nil {
		c.log.Error("clickhouse: campaign_state_changes query failed", "error", err)
		return nil
	}
	defer rows.Close()
	var out []CampaignStateChange
	for rows.Next() {
		var e CampaignStateChange
		if err := rows.Scan(&e.CampaignID, &e.AccountID, &e.OldState, &e.NewState, &e.Reason, &e.Timestamp); err != nil {
			c.log.Error("clickhouse: campaign_state_changes scan failed", "error", err)
			return out
		}
		out = append(out, e)
	}
	return out
}

func (c *ClickHouse) RenderFailuresByCreative(creativeID string) []RenderFailure {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := c.db.QueryContext(ctx,
		`SELECT trace_id, campaign_id, creative_id, placement_id, publisher_id, reason, detail, timestamp
		 FROM render_failures WHERE creative_id = ? ORDER BY timestamp`, creativeID)
	if err != nil {
		c.log.Error("clickhouse: render_failures query failed", "error", err)
		return nil
	}
	defer rows.Close()
	var out []RenderFailure
	for rows.Next() {
		var e RenderFailure
		if err := rows.Scan(&e.TraceID, &e.CampaignID, &e.CreativeID, &e.PlacementID, &e.PublisherID, &e.Reason, &e.Detail, &e.Timestamp); err != nil {
			c.log.Error("clickhouse: render_failures scan failed", "error", err)
			return out
		}
		out = append(out, e)
	}
	return out
}

func (c *ClickHouse) FreqCapBlocksByCampaign(campaignID string) []FreqCapBlock {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := c.db.QueryContext(ctx,
		`SELECT trace_id, user_id, campaign_id, placement_id, publisher_id, timestamp
		 FROM freq_cap_blocks WHERE campaign_id = ? ORDER BY timestamp`, campaignID)
	if err != nil {
		c.log.Error("clickhouse: freq_cap_blocks query failed", "error", err)
		return nil
	}
	defer rows.Close()
	var out []FreqCapBlock
	for rows.Next() {
		var e FreqCapBlock
		if err := rows.Scan(&e.TraceID, &e.UserID, &e.CampaignID, &e.PlacementID, &e.PublisherID, &e.Timestamp); err != nil {
			c.log.Error("clickhouse: freq_cap_blocks scan failed", "error", err)
			return out
		}
		out = append(out, e)
	}
	return out
}

func (c *ClickHouse) TrackerRejectionsByTrace(traceID, reason string) []TrackerRejection {
	q := `SELECT trace_id, event_type, reason, detail, timestamp FROM tracker_rejections WHERE trace_id = ?`
	args := []any{traceID}
	if reason != "" {
		q += ` AND reason = ?`
		args = append(args, reason)
	}
	q += ` ORDER BY timestamp`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := c.db.QueryContext(ctx, q, args...)
	if err != nil {
		c.log.Error("clickhouse: tracker_rejections query failed", "error", err)
		return nil
	}
	defer rows.Close()
	var out []TrackerRejection
	for rows.Next() {
		var e TrackerRejection
		if err := rows.Scan(&e.TraceID, &e.EventType, &e.Reason, &e.Detail, &e.Timestamp); err != nil {
			c.log.Error("clickhouse: tracker_rejections scan failed", "error", err)
			return out
		}
		out = append(out, e)
	}
	return out
}
