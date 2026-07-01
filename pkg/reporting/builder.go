// Package reporting provides the custom report builder and pre-built
// report templates for the analytics store.
//
// Reports are built as QueryParams, executed against the analytics store,
// and returned as structured results. The builder enforces multi-tenant
// filtering - every query is scoped to the requesting account.
//
// Usage:
//
//	b := reporting.NewBuilder(store)
//	report, err := b.
//	    Table("impressions").
//	    Metrics("count", "sum_cost").
//	    GroupBy("campaign_id", "day").
//	    Filter("account_id", accountID).
//	    TimeRange(from, to).
//	    Limit(100).
//	    Build(ctx)
package reporting

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/rollup"
)

// Builder constructs and executes analytics queries.
type Builder struct {
	store      analytics.Store
	table      string
	metrics    []string
	dimensions []string
	filters    map[string]string
	timeFrom   time.Time
	timeTo     time.Time
	limit      int
	orderBy    string
	orderDir   string
	autoTier   bool // auto-select rollup tier based on time range
}

// NewBuilder creates a report builder against the given analytics store.
func NewBuilder(store analytics.Store) *Builder {
	return &Builder{
		store:   store,
		filters: make(map[string]string),
	}
}

func (b *Builder) Table(t string) *Builder         { b.table = t; return b }
func (b *Builder) Metrics(m ...string) *Builder    { b.metrics = m; return b }
func (b *Builder) GroupBy(d ...string) *Builder    { b.dimensions = d; return b }
func (b *Builder) OrderByAsc(col string) *Builder  { b.orderBy = col; b.orderDir = "asc"; return b }
func (b *Builder) OrderByDesc(col string) *Builder { b.orderBy = col; b.orderDir = "desc"; return b }
func (b *Builder) Limit(n int) *Builder            { b.limit = n; return b }
func (b *Builder) AutoTier() *Builder              { b.autoTier = true; return b }

func (b *Builder) Filter(key, value string) *Builder {
	b.filters[key] = value
	return b
}

func (b *Builder) TimeRange(from, to time.Time) *Builder {
	b.timeFrom = from
	b.timeTo = to
	return b
}

// ForAccount scopes the query to a specific account (multi-tenancy).
func (b *Builder) ForAccount(accountID string) *Builder {
	b.filters["account_id"] = accountID
	return b
}

// tableToRollupConfig maps a raw source table to the rollup config that
// pre-aggregates it (rollup.EventsConfig / AuctionsConfig). A table absent here
// simply can't be tier-served and always reads raw.
var tableToRollupConfig = map[string]string{
	"impressions": "events",
	"auctions":    "auctions",
}

// rollupDimensions mirrors each rollup config's entity dimensions (must track
// the rollup.Config Dimensions). A query whose group-bys or filters reference a
// dimension not in this set can't be answered from rollups → raw fallback.
var rollupDimensions = map[string]map[string]bool{
	"events":   {"account_id": true, "publisher_id": true, "campaign_id": true, "creative_id": true, "placement_id": true, "geo": true, "device": true},
	"auctions": {"placement_id": true, "channel": true},
}

func isTimeDim(d string) bool { return d == "day" || d == "hour" }

// Build executes the query and returns the result.
//
// When AutoTier() is set and the store exposes RollupReader, Build selects a
// rollup tier from the time range (rollup.TierForRange) and re-aggregates the
// pre-computed rollup rows — cheap, and the reason rollups exist. It falls back
// to a raw scan whenever the query can't be served from rollups (unknown table,
// a group-by/filter/metric the rollup doesn't carry, or no matching rollup rows
// yet), so correctness never depends on the rollups being present.
func (b *Builder) Build(ctx context.Context) (*analytics.QueryResult, error) {
	if b.table == "" {
		return nil, fmt.Errorf("table is required")
	}

	if b.autoTier && !b.timeFrom.IsZero() && !b.timeTo.IsZero() {
		if reader, ok := b.store.(analytics.RollupReader); ok {
			if cfg, ok := tableToRollupConfig[b.table]; ok {
				tier := rollup.TierForRange(b.timeFrom, b.timeTo)
				if res, ok := b.buildFromRollups(ctx, reader, cfg, tier); ok {
					return res, nil
				}
			}
		}
	}

	params := analytics.QueryParams{
		Table:      b.table,
		Metrics:    b.metrics,
		Dimensions: b.dimensions,
		Filters:    b.filters,
		TimeFrom:   b.timeFrom,
		TimeTo:     b.timeTo,
		Limit:      b.limit,
		OrderBy:    b.orderBy,
		OrderDir:   b.orderDir,
	}

	return b.store.Query(ctx, params)
}

// buildFromRollups re-aggregates persisted rollup rows for the given config and
// tier into the requested group-bys + metrics. Returns (result, true) when the
// query is fully served from rollups; (nil, false) to signal "fall back to raw"
// — used when a dimension/filter/metric isn't supported or no rows match.
func (b *Builder) buildFromRollups(ctx context.Context, reader analytics.RollupReader, cfg string, tier rollup.Level) (*analytics.QueryResult, bool) {
	dims := rollupDimensions[cfg]

	// Every non-time group-by and every filter key must be an entity dimension
	// the rollup carries, else we can't reconstruct the answer from rollups.
	for _, d := range b.dimensions {
		if !isTimeDim(d) && !dims[d] {
			return nil, false
		}
	}
	for k := range b.filters {
		if !dims[k] {
			return nil, false
		}
	}
	// Only additive metrics (count, sum_*) survive re-aggregation across rows.
	// avg_* etc. would need count-weighting — fall back to raw for those.
	for _, m := range b.metrics {
		if m != "count" && !strings.HasPrefix(m, "sum_") {
			return nil, false
		}
	}

	rows, err := reader.QueryRollups(ctx, cfg, string(tier), b.timeFrom, b.timeTo)
	if err != nil || len(rows) == 0 {
		return nil, false
	}

	groups := map[string]map[string]float64{}
	var order []string
	for _, r := range rows {
		match := true
		for k, v := range b.filters {
			if r.Dimensions[k] != v {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		parts := make([]string, len(b.dimensions))
		for i, d := range b.dimensions {
			parts[i] = b.rollupDimValue(r, d)
		}
		key := strings.Join(parts, "\x00")
		if _, ok := groups[key]; !ok {
			groups[key] = map[string]float64{}
			order = append(order, key)
		}
		for _, m := range b.metrics {
			groups[key][m] += r.Metrics[m]
		}
	}
	if len(groups) == 0 {
		return nil, false // filtered to nothing — let raw answer authoritatively
	}

	cols := append(append([]string{}, b.dimensions...), b.metrics...)
	res := &analytics.QueryResult{Columns: cols}
	sort.Strings(order)
	for _, key := range order {
		var row []interface{}
		if len(b.dimensions) > 0 {
			for _, p := range strings.Split(key, "\x00") {
				row = append(row, p)
			}
		}
		for _, m := range b.metrics {
			v := groups[key][m]
			if m == "count" {
				row = append(row, int64(v))
			} else {
				row = append(row, v)
			}
		}
		res.Rows = append(res.Rows, row)
	}
	if b.limit > 0 && len(res.Rows) > b.limit {
		res.Rows = res.Rows[:b.limit]
	}
	return res, true
}

// rollupDimValue resolves a group-by dimension's value for a rollup row: time
// dimensions come from the window start, entity dimensions from the row's map.
func (b *Builder) rollupDimValue(r analytics.RollupRow, d string) string {
	switch d {
	case "day":
		return r.WindowFrom.Format("2006-01-02")
	case "hour":
		return r.WindowFrom.Format("2006-01-02T15")
	default:
		return r.Dimensions[d]
	}
}

// Report is a named, pre-built report that can be executed.
type Report struct {
	Name        string
	Description string
	BuildFn     func(b *Builder, accountID string, from, to time.Time) *Builder
}

// Execute runs a pre-built report for the given account and time range.
func (r *Report) Execute(ctx context.Context, store analytics.Store, accountID string, from, to time.Time) (*analytics.QueryResult, error) {
	b := NewBuilder(store)
	b = r.BuildFn(b, accountID, from, to)
	return b.Build(ctx)
}

// Pre-built report templates.
var (
	CampaignPerformance = &Report{
		Name:        "campaign_performance",
		Description: "Impressions, clicks, spend by campaign over time",
		BuildFn: func(b *Builder, accountID string, from, to time.Time) *Builder {
			return b.Table("impressions").
				Metrics("count", "sum_cost").
				GroupBy("campaign_id", "day").
				ForAccount(accountID).
				TimeRange(from, to).
				OrderByDesc("day")
		},
	}

	CreativePerformance = &Report{
		Name:        "creative_performance",
		Description: "Performance breakdown by creative",
		BuildFn: func(b *Builder, accountID string, from, to time.Time) *Builder {
			return b.Table("impressions").
				Metrics("count", "sum_cost").
				GroupBy("creative_id").
				ForAccount(accountID).
				TimeRange(from, to).
				OrderByDesc("count")
		},
	}

	GeoBreakdown = &Report{
		Name:        "geo_breakdown",
		Description: "Impressions and spend by geography",
		BuildFn: func(b *Builder, accountID string, from, to time.Time) *Builder {
			return b.Table("impressions").
				Metrics("count", "sum_cost").
				GroupBy("geo").
				ForAccount(accountID).
				TimeRange(from, to).
				OrderByDesc("sum_cost")
		},
	}

	PublisherYield = &Report{
		Name:        "publisher_yield",
		Description: "Revenue and fill rate by placement for publishers",
		BuildFn: func(b *Builder, accountID string, from, to time.Time) *Builder {
			return b.Table("impressions").
				Metrics("count", "sum_cost").
				GroupBy("placement_id", "day").
				Filter("publisher_id", accountID).
				TimeRange(from, to).
				OrderByDesc("day")
		},
	}
)

// AllReports returns all pre-built report templates.
func AllReports() []*Report {
	return []*Report{
		CampaignPerformance,
		CreativePerformance,
		GeoBreakdown,
		PublisherYield,
	}
}
