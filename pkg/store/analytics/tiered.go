package analytics

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ColdReader answers historical queries from cold storage (the Parquet data
// lake). It's implemented by pkg/store/datalake (behind the `duckdb` build tag);
// when cold isn't available it's nil and the TieredStore degrades to hot-only.
type ColdReader interface {
	Query(ctx context.Context, params QueryParams) (*QueryResult, error)
}

// TieredStore routes reads by time: rows within hotWindow of now come from the
// hot store (ClickHouse), older rows from cold (the Parquet lake). WRITES ALWAYS
// go to hot — the pipeline independently writes the lake, so the tiered store
// never writes cold. A query spanning the hot/cold boundary is split, run
// against both tiers, and merged additively by dimension key.
//
// It implements analytics.Store, so it drops in wherever a Store is expected
// (the reporting query path, the rollup Builder's raw fallback). The tenant
// scope in params.Filters rides through both sub-queries unchanged.
type TieredStore struct {
	hot       Store
	cold      ColdReader
	hotWindow time.Duration
	now       func() time.Time
	log       *slog.Logger
}

// NewTieredStore wraps a hot store with a cold reader. If cold is nil the store
// is hot-only (every read passes straight through). hotWindow is how far back
// the hot store is authoritative (e.g. 7d). log may be nil.
func NewTieredStore(hot Store, cold ColdReader, hotWindow time.Duration, log *slog.Logger) *TieredStore {
	if log == nil {
		log = slog.Default()
	}
	return &TieredStore{hot: hot, cold: cold, hotWindow: hotWindow, now: time.Now, log: log}
}

// Query routes by time range. Boundary = now − hotWindow; rows at or after it
// are hot, strictly before it are cold.
func (t *TieredStore) Query(ctx context.Context, params QueryParams) (*QueryResult, error) {
	if t.cold == nil {
		return t.hot.Query(ctx, params)
	}
	boundary := t.now().Add(-t.hotWindow)
	to := params.TimeTo
	if to.IsZero() {
		to = t.now()
	}

	// Entirely in one tier → single pass, native result (no float coercion).
	if !params.TimeFrom.IsZero() && !params.TimeFrom.Before(boundary) {
		return t.hot.Query(ctx, params) // from >= boundary
	}
	if !to.After(boundary) {
		return t.cold.Query(ctx, params) // to <= boundary
	}

	// Spanning the boundary. Non-additive metrics (avg_*) can't be merged across
	// tiers correctly, so serve them from hot only (documented approximation —
	// the derived-metric engine only ever asks for additive base metrics).
	if hasNonAdditive(params.Metrics) {
		t.log.Warn("tiered: non-additive metric over a hot/cold span — serving hot only (approximate)",
			"table", params.Table, "metrics", params.Metrics)
		return t.hot.Query(ctx, params)
	}

	// Split: hot owns [boundary, to], cold owns [from, boundary). Cold's upper
	// bound is exclusive (boundary − 1ns) so a row exactly at the boundary is
	// counted once (hot), never double-counted. Limit/order are dropped on the
	// sub-queries and re-applied to the merged result.
	hotParams := params
	hotParams.TimeFrom = boundary
	hotParams.Limit, hotParams.OrderBy, hotParams.OrderDir = 0, "", ""

	coldParams := params
	coldParams.TimeTo = boundary.Add(-time.Nanosecond)
	coldParams.Limit, coldParams.OrderBy, coldParams.OrderDir = 0, "", ""

	hotRes, err := t.hot.Query(ctx, hotParams)
	if err != nil {
		return nil, err
	}
	coldRes, err := t.cold.Query(ctx, coldParams)
	if err != nil {
		// Cold is best-effort (S3/DuckDB may be down): degrade to the hot half
		// rather than fail the whole query. Logged so staleness is visible.
		t.log.Warn("tiered: cold query failed, serving hot window only", "table", params.Table, "error", err)
		coldRes = &QueryResult{}
	}

	merged := mergeAdditive(params.Dimensions, params.Metrics, hotRes, coldRes)
	applyTieredOrderLimit(merged, params.OrderBy, params.OrderDir, params.Limit)
	return merged, nil
}

// Write path + lifecycle all delegate to hot (the lake is written by the pipeline).
func (t *TieredStore) InsertImpression(ctx context.Context, e *ImpressionEvent) error {
	return t.hot.InsertImpression(ctx, e)
}
func (t *TieredStore) InsertClick(ctx context.Context, e *ClickEvent) error {
	return t.hot.InsertClick(ctx, e)
}
func (t *TieredStore) InsertConversion(ctx context.Context, e *ConversionEvent) error {
	return t.hot.InsertConversion(ctx, e)
}
func (t *TieredStore) InsertView(ctx context.Context, e *ViewEvent) error {
	return t.hot.InsertView(ctx, e)
}
func (t *TieredStore) InsertAuction(ctx context.Context, e *AuctionEvent) error {
	return t.hot.InsertAuction(ctx, e)
}
func (t *TieredStore) InsertAuctionWin(ctx context.Context, e *AuctionWinEvent) error {
	return t.hot.InsertAuctionWin(ctx, e)
}
func (t *TieredStore) InsertMediaEvent(ctx context.Context, e *MediaEvent) error {
	return t.hot.InsertMediaEvent(ctx, e)
}
func (t *TieredStore) InsertBatch(ctx context.Context, events []Event) error {
	return t.hot.InsertBatch(ctx, events)
}

// Close closes the hot store; if the cold reader owns a closable handle it's
// closed too (idempotent — the caller may also close it).
func (t *TieredStore) Close() error {
	if c, ok := t.cold.(interface{ Close() error }); ok {
		_ = c.Close()
	}
	return t.hot.Close()
}

// additiveMetrics are the base metrics that sum across tiers/rollups. avg_* are
// deliberately absent — a mean can't be re-derived by adding two means.
var additiveMetrics = map[string]bool{
	"count": true, "sum_cost": true, "sum_revenue": true, "sum_bids": true,
}

func hasNonAdditive(metrics []string) bool {
	for _, m := range metrics {
		if !additiveMetrics[m] {
			return true
		}
	}
	return false
}

// mergeAdditive folds one or more results into one, summing metric columns by
// dimension-key. Column order is canonical: dimensions then metrics. Values come
// out as float64 (the only type that survives summing int + float rows from two
// backends); the reporting engine and JSON both accept that.
func mergeAdditive(dims, metrics []string, parts ...*QueryResult) *QueryResult {
	type acc struct {
		dimVals []interface{}
		mets    map[string]float64
	}
	accs := map[string]*acc{}
	var order []string

	for _, part := range parts {
		if part == nil {
			continue
		}
		colIdx := map[string]int{}
		for i, c := range part.Columns {
			colIdx[c] = i
		}
		for _, row := range part.Rows {
			dimVals := make([]interface{}, len(dims))
			keyParts := make([]string, len(dims))
			for i, d := range dims {
				if ci, ok := colIdx[d]; ok && ci < len(row) {
					dimVals[i] = row[ci]
					keyParts[i] = toStringKey(row[ci])
				}
			}
			key := strings.Join(keyParts, "\x00")
			a := accs[key]
			if a == nil {
				a = &acc{dimVals: dimVals, mets: map[string]float64{}}
				accs[key] = a
				order = append(order, key)
			}
			for _, m := range metrics {
				if ci, ok := colIdx[m]; ok && ci < len(row) {
					a.mets[m] += toFloatVal(row[ci])
				}
			}
		}
	}

	columns := append(append([]string{}, dims...), metrics...)
	res := &QueryResult{Columns: columns}
	for _, key := range order {
		a := accs[key]
		row := append([]interface{}{}, a.dimVals...)
		for _, m := range metrics {
			row = append(row, a.mets[m])
		}
		res.Rows = append(res.Rows, row)
	}
	return res
}

// applyTieredOrderLimit sorts the merged rows by a metric column and truncates —
// order/limit can't be pushed down when two tiers are merged, so it's applied
// once on the combined result.
func applyTieredOrderLimit(res *QueryResult, orderBy, orderDir string, limit int) {
	if res == nil {
		return
	}
	if orderBy != "" {
		ci := -1
		for i, c := range res.Columns {
			if c == orderBy {
				ci = i
				break
			}
		}
		if ci >= 0 {
			sort.SliceStable(res.Rows, func(i, j int) bool {
				a, b := toFloatVal(res.Rows[i][ci]), toFloatVal(res.Rows[j][ci])
				if orderDir == "desc" {
					return a > b
				}
				return a < b
			})
		}
	}
	if limit > 0 && len(res.Rows) > limit {
		res.Rows = res.Rows[:limit]
	}
}

func toStringKey(v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return strconv.FormatFloat(toFloatVal(v), 'g', -1, 64)
}

func toFloatVal(v interface{}) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case int32:
		return float64(n)
	case uint64:
		return float64(n)
	case uint32:
		return float64(n)
	case uint:
		return float64(n)
	case string:
		f, _ := strconv.ParseFloat(n, 64)
		return f
	default:
		return 0
	}
}
