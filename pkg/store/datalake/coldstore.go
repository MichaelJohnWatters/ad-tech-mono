//go:build duckdb

package datalake

import (
	"context"
	"fmt"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

// ColdStore adapts the Parquet data lake to analytics.ColdReader: it maps a
// QueryParams to SQL via the SAME analytics.BuildQueryFrom aggregate logic as
// the hot store, so the cold tier computes byte-identical columns and applies
// the identical tenant WHERE — the two properties the TieredStore relies on to
// merge across the boundary and to never leak across tenants.
//
// It reads via DuckDB delta_scan() over the table root. Our writer now emits a
// real Delta transaction log (see deltalog.go / objstore.go), so delta_scan
// resolves the active file set from the log itself — correct AND tombstone-aware
// after compaction, with no separate active-file plumbing.
//
// Only compiled with the `duckdb` build tag (CGO). Without it, reporting wires
// cold as nil and degrades to hot-only.
type ColdStore struct {
	reader *ParquetReader
}

// NewColdStore wraps an open ParquetReader (the DuckDB SQL engine over the lake).
func NewColdStore(reader *ParquetReader) *ColdStore { return &ColdStore{reader: reader} }

// coldTables is the allowlist of tables the cold tier will scan. params.Table is
// interpolated into the delta_scan URI (not a bind parameter), so it MUST be
// validated against this set — never let an arbitrary string reach the FROM
// expression.
var coldTables = map[string]bool{
	"impressions":  true,
	"clicks":       true,
	"conversions":  true,
	"views":        true,
	"auctions":     true,
	"auction_wins": true,
	"media_events": true,
}

// Query aggregates over the lake and returns a positional QueryResult whose
// columns match the hot store's (dimensions then metrics). A table not yet in
// the lake (no _delta_log) yields zero rows rather than an error, so a
// boundary-spanning query just gets the hot half for that table.
func (c *ColdStore) Query(ctx context.Context, params analytics.QueryParams) (*analytics.QueryResult, error) {
	if !coldTables[params.Table] {
		return nil, fmt.Errorf("cold: unknown table %q", params.Table)
	}

	columns := append([]string{}, params.Dimensions...)
	if len(params.Metrics) == 0 {
		columns = append(columns, "count")
	} else {
		columns = append(columns, params.Metrics...)
	}
	empty := &analytics.QueryResult{Columns: columns}

	if !c.reader.tableExists(ctx, params.Table) {
		return empty, nil
	}

	fromExpr := fmt.Sprintf("delta_scan('s3://%s/%s')", c.reader.bucket, params.Table)
	query, args := analytics.BuildQueryFrom(params, fromExpr)

	maps, err := c.reader.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("cold query %s: %w", params.Table, err)
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
