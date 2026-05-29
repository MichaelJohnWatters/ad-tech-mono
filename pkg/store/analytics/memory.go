package analytics

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// MemoryStore is an in-memory analytics store for testing.
// Not suitable for production - no persistence, no SQL.
type MemoryStore struct {
	mu          sync.RWMutex
	impressions []ImpressionEvent
	clicks      []ClickEvent
	conversions []ConversionEvent
	auctions    []AuctionEvent
}

// NewMemory creates an in-memory analytics store.
func NewMemory() *MemoryStore {
	return &MemoryStore{}
}

func (s *MemoryStore) InsertImpression(_ context.Context, e *ImpressionEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.impressions = append(s.impressions, *e)
	return nil
}

func (s *MemoryStore) InsertClick(_ context.Context, e *ClickEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clicks = append(s.clicks, *e)
	return nil
}

func (s *MemoryStore) InsertConversion(_ context.Context, e *ConversionEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conversions = append(s.conversions, *e)
	return nil
}

func (s *MemoryStore) InsertAuction(_ context.Context, e *AuctionEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auctions = append(s.auctions, *e)
	return nil
}

func (s *MemoryStore) InsertBatch(ctx context.Context, events []Event) error {
	for _, e := range events {
		var err error
		switch e.Type {
		case EventImpression:
			err = s.InsertImpression(ctx, e.Impression)
		case EventClick:
			err = s.InsertClick(ctx, e.Click)
		case EventConversion:
			err = s.InsertConversion(ctx, e.Conversion)
		case EventAuction:
			err = s.InsertAuction(ctx, e.Auction)
		default:
			return fmt.Errorf("unknown event type: %s", e.Type)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *MemoryStore) Query(_ context.Context, params QueryParams) (*QueryResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	switch params.Table {
	case "impressions":
		return s.queryImpressions(params)
	case "clicks":
		return s.queryClicks(params)
	case "conversions":
		return s.queryConversions(params)
	case "auctions":
		return s.queryAuctions(params)
	default:
		return nil, fmt.Errorf("unknown table: %s", params.Table)
	}
}

func (s *MemoryStore) Close() error { return nil }

// Counts returns event counts for assertions in tests.
func (s *MemoryStore) Counts() (impressions, clicks, conversions, auctions int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.impressions), len(s.clicks), len(s.conversions), len(s.auctions)
}

// Impressions returns all stored impressions for test assertions.
func (s *MemoryStore) Impressions() []ImpressionEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ImpressionEvent, len(s.impressions))
	copy(out, s.impressions)
	return out
}

func (s *MemoryStore) queryImpressions(params QueryParams) (*QueryResult, error) {
	// Filter by time range and filters
	var filtered []ImpressionEvent
	for _, imp := range s.impressions {
		if !params.TimeFrom.IsZero() && imp.Timestamp.Before(params.TimeFrom) {
			continue
		}
		if !params.TimeTo.IsZero() && imp.Timestamp.After(params.TimeTo) {
			continue
		}
		if !matchFilters(params.Filters, map[string]string{
			"trace_id":     imp.TraceID,
			"campaign_id":  imp.CampaignID,
			"creative_id":  imp.CreativeID,
			"placement_id": imp.PlacementID,
			"publisher_id": imp.PublisherID,
			"account_id":   imp.AccountID,
			"geo":          imp.Geo,
			"device":       imp.Device,
			"channel":      imp.Channel,
		}) {
			continue
		}
		filtered = append(filtered, imp)
	}

	// If no dimensions, aggregate across all filtered rows
	if len(params.Dimensions) == 0 {
		return s.aggregateImpressions(filtered, params.Metrics, nil)
	}

	// Group by dimensions
	groups := map[string][]ImpressionEvent{}
	for _, imp := range filtered {
		key := dimensionKey(params.Dimensions, map[string]string{
			"trace_id":     imp.TraceID,
			"campaign_id":  imp.CampaignID,
			"creative_id":  imp.CreativeID,
			"placement_id": imp.PlacementID,
			"publisher_id": imp.PublisherID,
			"account_id":   imp.AccountID,
			"geo":          imp.Geo,
			"device":       imp.Device,
			"channel":      imp.Channel,
			"day":          imp.Timestamp.Format("2006-01-02"),
			"hour":         imp.Timestamp.Format("2006-01-02T15"),
		})
		groups[key] = append(groups[key], imp)
	}

	return s.aggregateImpressionGroups(groups, params)
}

func (s *MemoryStore) aggregateImpressions(imps []ImpressionEvent, metrics []string, dimValues []string) (*QueryResult, error) {
	cols := make([]string, 0, len(dimValues)+len(metrics))
	row := make([]interface{}, 0, len(dimValues)+len(metrics))

	for _, v := range dimValues {
		row = append(row, v)
	}

	for _, m := range metrics {
		switch m {
		case "count":
			cols = append(cols, "count")
			row = append(row, int64(len(imps)))
		case "sum_cost":
			cols = append(cols, "sum_cost")
			var sum float64
			for _, imp := range imps {
				sum += imp.ClearingPriceUSD
			}
			row = append(row, sum)
		default:
			return nil, fmt.Errorf("unknown metric: %s", m)
		}
	}

	return &QueryResult{Columns: cols, Rows: [][]interface{}{row}}, nil
}

func (s *MemoryStore) aggregateImpressionGroups(groups map[string][]ImpressionEvent, params QueryParams) (*QueryResult, error) {
	var columns []string
	columns = append(columns, params.Dimensions...)
	columns = append(columns, params.Metrics...)

	var rows [][]interface{}
	for key, imps := range groups {
		dimValues := strings.Split(key, "|")
		row := make([]interface{}, 0, len(dimValues)+len(params.Metrics))
		for _, v := range dimValues {
			row = append(row, v)
		}
		for _, m := range params.Metrics {
			switch m {
			case "count":
				row = append(row, int64(len(imps)))
			case "sum_cost":
				var sum float64
				for _, imp := range imps {
					sum += imp.ClearingPriceUSD
				}
				row = append(row, sum)
			}
		}
		rows = append(rows, row)
	}

	// Sort for deterministic test output
	sort.Slice(rows, func(i, j int) bool {
		return fmt.Sprint(rows[i][0]) < fmt.Sprint(rows[j][0])
	})

	if params.Limit > 0 && len(rows) > params.Limit {
		rows = rows[:params.Limit]
	}

	return &QueryResult{Columns: columns, Rows: rows}, nil
}

func (s *MemoryStore) queryClicks(params QueryParams) (*QueryResult, error) {
	var count int64
	for _, c := range s.clicks {
		if !params.TimeFrom.IsZero() && c.Timestamp.Before(params.TimeFrom) {
			continue
		}
		if !params.TimeTo.IsZero() && c.Timestamp.After(params.TimeTo) {
			continue
		}
		if !matchFilters(params.Filters, map[string]string{
			"trace_id":     c.TraceID,
			"campaign_id":  c.CampaignID,
			"placement_id": c.PlacementID,
			"account_id":   c.AccountID,
		}) {
			continue
		}
		count++
	}
	return &QueryResult{
		Columns: []string{"count"},
		Rows:    [][]interface{}{{count}},
	}, nil
}

func (s *MemoryStore) queryConversions(params QueryParams) (*QueryResult, error) {
	var count int64
	var totalRevenue float64
	for _, c := range s.conversions {
		if !params.TimeFrom.IsZero() && c.Timestamp.Before(params.TimeFrom) {
			continue
		}
		if !params.TimeTo.IsZero() && c.Timestamp.After(params.TimeTo) {
			continue
		}
		if !matchFilters(params.Filters, map[string]string{
			"trace_id":    c.TraceID,
			"campaign_id": c.CampaignID,
			"account_id":  c.AccountID,
		}) {
			continue
		}
		count++
		totalRevenue += c.RevenueUSD
	}
	return &QueryResult{
		Columns: []string{"count", "sum_revenue"},
		Rows:    [][]interface{}{{count, totalRevenue}},
	}, nil
}

func (s *MemoryStore) queryAuctions(params QueryParams) (*QueryResult, error) {
	var count int64
	var totalDuration int64
	for _, a := range s.auctions {
		if !params.TimeFrom.IsZero() && a.Timestamp.Before(params.TimeFrom) {
			continue
		}
		if !params.TimeTo.IsZero() && a.Timestamp.After(params.TimeTo) {
			continue
		}
		if !matchFilters(params.Filters, map[string]string{
			"trace_id":     a.TraceID,
			"placement_id": a.PlacementID,
			"publisher_id": a.PublisherID,
			"channel":      a.Channel,
		}) {
			continue
		}
		count++
		totalDuration += a.DurationMs
	}
	var avgDuration float64
	if count > 0 {
		avgDuration = float64(totalDuration) / float64(count)
	}
	return &QueryResult{
		Columns: []string{"count", "avg_duration_ms"},
		Rows:    [][]interface{}{{count, avgDuration}},
	}, nil
}

func matchFilters(filters map[string]string, fields map[string]string) bool {
	for k, v := range filters {
		if fv, ok := fields[k]; ok && fv != v {
			return false
		}
	}
	return true
}

func dimensionKey(dims []string, fields map[string]string) string {
	parts := make([]string, len(dims))
	for i, d := range dims {
		parts[i] = fields[d]
	}
	return strings.Join(parts, "|")
}

// timeToDay and timeToHour for dimension grouping
func timeToDay(t time.Time) string  { return t.Format("2006-01-02") }
func timeToHour(t time.Time) string { return t.Format("2006-01-02T15") }
