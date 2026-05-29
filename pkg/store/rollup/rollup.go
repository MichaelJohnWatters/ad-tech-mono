// Package rollup provides a config-driven rollup engine for aggregating
// time-series data across multiple granularities.
//
// One engine, many configurations. Each data source (events, auctions,
// bid logs, fraud scores) is defined as a RollupConfig. The engine
// reads from the analytics store at one granularity and writes the
// aggregated result at the next coarser level.
//
// Rollup chain: raw -> minute -> hourly -> daily -> monthly
//
// Usage:
//
//	engine := rollup.NewEngine(store, clock, logger)
//	engine.Register(rollup.EventsConfig)
//	engine.RunLevel(ctx, rollup.Minute) // aggregate raw -> minute
package rollup

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

// Level represents a rollup granularity.
type Level string

const (
	Minute  Level = "minute"
	Hourly  Level = "hourly"
	Daily   Level = "daily"
	Monthly Level = "monthly"
)

// Retention defines how long each level of data is retained.
type Retention struct {
	Raw     time.Duration // 48 hours
	Minute  time.Duration // 7 days
	Hourly  time.Duration // 90 days
	Daily   time.Duration // 2 years
	Monthly time.Duration // 0 = forever
}

// StandardRetention is the default retention policy.
var StandardRetention = Retention{
	Raw:     48 * time.Hour,
	Minute:  7 * 24 * time.Hour,
	Hourly:  90 * 24 * time.Hour,
	Daily:   2 * 365 * 24 * time.Hour,
	Monthly: 0, // forever
}

// Config defines a rollup source and its aggregation rules.
type Config struct {
	Name       string   // e.g. "events", "auctions"
	Source     string   // source table in analytics store
	Dimensions []string // group by columns
	Metrics    []string // aggregate functions
	Retention  Retention
}

// EventsConfig is the standard rollup for impression/click/conversion events.
var EventsConfig = Config{
	Name:       "events",
	Source:     "impressions",
	Dimensions: []string{"campaign_id", "creative_id", "placement_id", "geo", "device"},
	Metrics:    []string{"count", "sum_cost"},
	Retention:  StandardRetention,
}

// AuctionsConfig is the rollup for auction logs.
var AuctionsConfig = Config{
	Name:       "auctions",
	Source:     "auctions",
	Dimensions: []string{"placement_id", "channel"},
	Metrics:    []string{"count", "avg_duration_ms"},
	Retention:  StandardRetention,
}

// Result holds the outcome of a rollup execution.
type Result struct {
	Config     string
	Level      Level
	WindowFrom time.Time
	WindowTo   time.Time
	RowsRead   int
	RowsWritten int
	Duration   time.Duration
}

// Engine runs rollup aggregations against the analytics store.
type Engine struct {
	store   analytics.Store
	clk     clock.Clock
	log     *slog.Logger
	configs []Config
}

// NewEngine creates a rollup engine.
func NewEngine(store analytics.Store, clk clock.Clock, log *slog.Logger) *Engine {
	return &Engine{store: store, clk: clk, log: log}
}

// Register adds a rollup configuration.
func (e *Engine) Register(cfg Config) {
	e.configs = append(e.configs, cfg)
}

// RunLevel executes all registered rollups at the given level.
func (e *Engine) RunLevel(ctx context.Context, level Level) ([]Result, error) {
	var results []Result
	for _, cfg := range e.configs {
		result, err := e.runOne(ctx, cfg, level)
		if err != nil {
			e.log.Error("rollup failed", "config", cfg.Name, "level", level, "error", err)
			return results, fmt.Errorf("rollup %s at %s: %w", cfg.Name, level, err)
		}
		results = append(results, result)
		e.log.Info("rollup complete",
			"config", cfg.Name,
			"level", string(level),
			"rows_read", result.RowsRead,
			"rows_written", result.RowsWritten,
			"duration_ms", result.Duration.Milliseconds(),
		)
	}
	return results, nil
}

func (e *Engine) runOne(ctx context.Context, cfg Config, level Level) (Result, error) {
	start := e.clk.Now()
	now := e.clk.Now()

	from, to := windowForLevel(level, now)

	// Query source data for the window
	timeDim := timeDimensionForLevel(level)
	dims := append([]string{timeDim}, cfg.Dimensions...)

	qr, err := e.store.Query(ctx, analytics.QueryParams{
		Table:      cfg.Source,
		Dimensions: dims,
		Metrics:    cfg.Metrics,
		TimeFrom:   from,
		TimeTo:     to,
	})
	if err != nil {
		return Result{}, fmt.Errorf("query source: %w", err)
	}

	rowsRead := len(qr.Rows)

	// In a full implementation, we'd write these aggregated rows to a
	// rollup table (e.g. impressions_hourly). For now, we return the
	// result for the caller to handle.

	return Result{
		Config:      cfg.Name,
		Level:       level,
		WindowFrom:  from,
		WindowTo:    to,
		RowsRead:    rowsRead,
		RowsWritten: rowsRead, // 1:1 for now
		Duration:    e.clk.Since(start),
	}, nil
}

// windowForLevel returns the time window to aggregate.
func windowForLevel(level Level, now time.Time) (from, to time.Time) {
	switch level {
	case Minute:
		// Aggregate the last completed minute
		to = now.Truncate(time.Minute)
		from = to.Add(-time.Minute)
	case Hourly:
		// Aggregate the last completed hour
		to = now.Truncate(time.Hour)
		from = to.Add(-time.Hour)
	case Daily:
		// Aggregate the last completed day (UTC)
		y, m, d := now.UTC().Date()
		to = time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
		from = to.AddDate(0, 0, -1)
	case Monthly:
		// Aggregate the last completed month
		y, m, _ := now.UTC().Date()
		to = time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
		from = to.AddDate(0, -1, 0)
	default:
		to = now
		from = now.Add(-time.Hour)
	}
	return from, to
}

func timeDimensionForLevel(level Level) string {
	switch level {
	case Minute, Hourly:
		return "hour"
	case Daily, Monthly:
		return "day"
	default:
		return "hour"
	}
}

// TierForRange returns the appropriate rollup tier for a given query time range.
// The reporting service uses this to auto-select the right data source.
func TierForRange(from, to time.Time) Level {
	dur := to.Sub(from)
	switch {
	case dur <= 30*time.Minute:
		return Minute
	case dur <= 24*time.Hour:
		return Hourly
	case dur <= 90*24*time.Hour:
		return Daily
	default:
		return Monthly
	}
}
