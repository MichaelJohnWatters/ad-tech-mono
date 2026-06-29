//go:build duckdb

package main

import "github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"

// newDuckDBStore opens the embedded DuckDB analytics store. Compiled only
// with the `duckdb` build tag (the marcboeker/go-duckdb driver is CGO, so
// the default CGO_ENABLED=0 build excludes it). Build reporting with
// `CGO_ENABLED=1 go build -tags duckdb ./cmd/reporting`.
func newDuckDBStore(path string) (analytics.Store, error) {
	return analytics.NewDuckDB(path)
}
