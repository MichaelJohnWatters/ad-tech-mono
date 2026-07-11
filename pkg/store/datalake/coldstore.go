//go:build duckdb

package datalake

import (
	"context"
	"fmt"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

// ColdStore adapts a ParquetReader to analytics.ColdReader: it maps a
// QueryParams to SQL over delta_scan('s3://…/<table>') using the SAME
// analytics.BuildQueryFrom aggregate logic as the hot store. Because the SELECT,
// WHERE (tenant filter + time), GROUP BY, ORDER, and LIMIT are all built by the
// shared function, the cold tier computes byte-identical columns and applies the
// identical tenant scope as ClickHouse — the two properties the TieredStore
// relies on to merge across the boundary and to never leak across tenants.
//
// Only compiled with the `duckdb` build tag (CGO). When the binary is built
// without it, reporting wires cold as nil and degrades to hot-only.
type ColdStore struct {
	reader *ParquetReader
}

// NewColdStore wraps an open ParquetReader. The reader owns the DuckDB handle;
// Close closes it.
func NewColdStore(reader *ParquetReader) *ColdStore { return &ColdStore{reader: reader} }

// coldTables is the allowlist of tables the cold tier will scan. params.Table is
// interpolated into the FROM expression (not a bind parameter), so it MUST be
// validated against this set — never let an arbitrary string reach delta_scan.
var coldTables = map[string]bool{
	"impressions":  true,
	"clicks":       true,
	"conversions":  true,
	"views":        true,
	"auctions":     true,
	"auction_wins": true,
	"media_events": true,
}

// Query runs the aggregate over the Parquet lake and returns a positional
// QueryResult whose columns match the hot store's (dimensions then metrics).
func (c *ColdStore) Query(ctx context.Context, params analytics.QueryParams) (*analytics.QueryResult, error) {
	if !coldTables[params.Table] {
		return nil, fmt.Errorf("cold: unknown table %q", params.Table)
	}
	fromExpr := fmt.Sprintf("delta_scan('%s')", c.reader.DeltaScanURI(params.Table))
	query, args := analytics.BuildQueryFrom(params, fromExpr)

	maps, err := c.reader.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("cold query %s: %w", params.Table, err)
	}

	// Canonical column order, mirroring BuildQueryFrom's SELECT: dimensions then
	// metrics (defaulting to a single count when no metrics were requested).
	columns := append([]string{}, params.Dimensions...)
	if len(params.Metrics) == 0 {
		columns = append(columns, "count")
	} else {
		columns = append(columns, params.Metrics...)
	}

	res := &analytics.QueryResult{Columns: columns}
	for _, m := range maps {
		row := make([]interface{}, len(columns))
		for i, col := range columns {
			row[i] = m[col]
		}
		res.Rows = append(res.Rows, row)
	}
	return res, nil
}

// Close releases the underlying DuckDB handle.
func (c *ColdStore) Close() error { return c.reader.Close() }
