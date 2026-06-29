//go:build !duckdb

package main

import (
	"errors"
	"log/slog"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

// newDuckDBStore is the stub compiled into the default (CGO-free) build.
// Requesting reporting.analytics_backend=duckdb without the build tag is
// a config error: the binary physically has no DuckDB driver linked in.
func newDuckDBStore(_ string, _ *slog.Logger) (analytics.Store, error) {
	return nil, errors.New("reporting was built without the 'duckdb' build tag; rebuild with `CGO_ENABLED=1 go build -tags duckdb ./cmd/reporting`")
}
