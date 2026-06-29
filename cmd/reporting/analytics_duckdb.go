//go:build duckdb

package main

import (
	"log/slog"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

// newDuckDBStore opens the embedded DuckDB analytics store. Compiled only
// with the `duckdb` build tag (the marcboeker/go-duckdb driver is CGO, so
// the default CGO_ENABLED=0 build excludes it). Build reporting with
// `CGO_ENABLED=1 go build -tags duckdb ./cmd/reporting`.
func newDuckDBStore(path string, log *slog.Logger) (analytics.Store, error) {
	store, err := analytics.NewDuckDB(path)
	if err != nil {
		return nil, err
	}
	store.SetLogger(log)
	return store, nil
}
