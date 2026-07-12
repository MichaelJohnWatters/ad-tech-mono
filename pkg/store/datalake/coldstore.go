//go:build duckdb

package datalake

import (
	"context"
	"fmt"
	"strings"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

// ColdStore adapts the Parquet data lake to analytics.ColdReader: it maps a
// QueryParams to SQL via the SAME analytics.BuildQueryFrom aggregate logic as
// the hot store, so the cold tier computes byte-identical columns and applies
// the identical tenant WHERE — the two properties the TieredStore relies on to
// merge across the boundary and to never leak across tenants.
//
// It resolves the ACTIVE file set from our Delta log (Snapshot.ActiveFiles) and
// reads exactly those parts with DuckDB read_parquet([...]). That is correct
// with our home-grown log format AND tombstone-aware (compacted-away files are
// excluded, so no double-count) — unlike a blind read_parquet('.../*.parquet')
// glob, and unlike delta_scan(), which needs a real Delta protocol/metaData log
// our writer doesn't emit.
//
// Only compiled with the `duckdb` build tag (CGO). Without it, reporting wires
// cold as nil and degrades to hot-only.
type ColdStore struct {
	reader *ParquetReader
	lake   *ObjectStore
}

// NewColdStore wraps an open ParquetReader (the DuckDB SQL engine) and the
// ObjectStore over the same bucket (resolves the active file set from the log).
func NewColdStore(reader *ParquetReader, lake *ObjectStore) *ColdStore {
	return &ColdStore{reader: reader, lake: lake}
}

// coldTables is the allowlist of tables the cold tier will scan. params.Table is
// interpolated into object paths (not a bind parameter), so it MUST be validated
// against this set — never let an arbitrary string reach the FROM expression.
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
// columns match the hot store's (dimensions then metrics). A table that isn't in
// the lake yet (empty/absent log) is not an error — it yields zero rows, so a
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

	snap, err := c.lake.Snapshot(ctx, params.Table)
	if err != nil {
		// Table absent from the lake (or its log unreadable) → no cold rows.
		return empty, nil
	}
	if len(snap.ActiveFiles) == 0 {
		return empty, nil
	}

	uris := make([]string, len(snap.ActiveFiles))
	for i, f := range snap.ActiveFiles {
		uris[i] = fmt.Sprintf("'s3://%s/%s'", c.reader.bucket, f)
	}
	fromExpr := fmt.Sprintf("read_parquet([%s])", strings.Join(uris, ", "))
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
