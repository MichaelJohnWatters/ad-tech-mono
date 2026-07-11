package reporting

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

// querier is the read surface the engine composes over — satisfied by
// analytics.Store, the tiered router, or any wrapper. Keeping it minimal lets
// the engine sit above whatever storage/rollup layering exists.
type querier interface {
	Query(ctx context.Context, params analytics.QueryParams) (*analytics.QueryResult, error)
}

// QueryEngine computes derived metrics (ecpm/ctr/fill_rate/net_revenue)
// SERVER-SIDE so clients only render. It's a drop-in for store.Query: a query
// with no derived metrics passes straight through unchanged (the raw path stays
// byte-identical). Derived metrics are composed from base queries — each base
// query carries the SAME params.Filters, so the gateway's tenant scope rides
// through every sub-query (no cross-tenant leak).
type QueryEngine struct {
	store querier
	net   NetResolver
}

// NewQueryEngine wraps a store (+ optional net resolver for net_revenue).
func NewQueryEngine(store querier, net NetResolver) *QueryEngine {
	return &QueryEngine{store: store, net: net}
}

// Query resolves the request. No derived metrics → transparent pass-through.
func (e *QueryEngine) Query(ctx context.Context, params analytics.QueryParams) (*analytics.QueryResult, error) {
	hasDerived := false
	for _, m := range params.Metrics {
		if isDerived(m) {
			hasDerived = true
			break
		}
	}
	if !hasDerived {
		return e.baseQuery(ctx, params)
	}
	return e.resolveDerived(ctx, params)
}

// baseQuery runs one backing query. Phase 2 wraps this with an AutoTier Builder
// so rollups become reachable; today it's the raw store.
func (e *QueryEngine) baseQuery(ctx context.Context, params analytics.QueryParams) (*analytics.QueryResult, error) {
	return e.store.Query(ctx, params)
}

// tableData indexes one table's result by dimension-key.
type tableData struct {
	dimVals map[string][]interface{}      // key -> dimension values (original)
	vals    map[string]map[string]float64 // key -> base metric -> value
	order   []string                      // key order as returned
}

func (e *QueryEngine) resolveDerived(ctx context.Context, params analytics.QueryParams) (*analytics.QueryResult, error) {
	// 1. Collect the base (table, metric) sources needed: user's own base
	//    metrics against the primary table, plus each derived metric's sources.
	tableMetrics := map[string]map[string]bool{}
	addSrc := func(table, metric string) {
		if tableMetrics[table] == nil {
			tableMetrics[table] = map[string]bool{}
		}
		tableMetrics[table][metric] = true
	}
	needsPublisher := false
	for _, m := range params.Metrics {
		if d, ok := derivedMetrics[m]; ok {
			for _, s := range d.sources {
				addSrc(s.table, s.metric)
			}
			if d.needsPublisher {
				needsPublisher = true
			}
		} else {
			addSrc(params.Table, m) // base metric on the primary table
		}
	}

	// 2. net_revenue requires publisher scope (group-by or single filter) so we
	//    never blend the fallback fee across mixed publishers.
	pubDim := contains(params.Dimensions, "publisher_id")
	pubFilter := params.Filters["publisher_id"]
	if needsPublisher && !pubDim && pubFilter == "" {
		return nil, fmt.Errorf("net_revenue requires publisher scope (group by publisher_id or filter by a single publisher_id)")
	}

	// 3. Run one query per source table (dims/filters/time identical → tenant
	//    scope preserved). Order/limit are applied after composition.
	results := map[string]*tableData{}
	for table, mset := range tableMetrics {
		sub := params
		sub.Table = table
		sub.Metrics = sortedKeys(mset)
		sub.Limit = 0
		sub.OrderBy = ""
		sub.OrderDir = ""
		res, err := e.baseQuery(ctx, sub)
		if err != nil {
			return nil, fmt.Errorf("base query %s: %w", table, err)
		}
		results[table] = indexResult(res, params.Dimensions)
	}

	// 4. Union of dimension keys — primary table first for stable ordering.
	primary := params.Table
	var keyOrder []string
	seen := map[string]bool{}
	appendKeys := func(td *tableData) {
		if td == nil {
			return
		}
		for _, k := range td.order {
			if !seen[k] {
				seen[k] = true
				keyOrder = append(keyOrder, k)
			}
		}
	}
	appendKeys(results[primary])
	for t, td := range results {
		if t != primary {
			appendKeys(td)
		}
	}

	// 5. Build output rows: dimensions + requested metrics in request order.
	columns := append([]string{}, params.Dimensions...)
	columns = append(columns, params.Metrics...)
	pubIdx := indexOf(params.Dimensions, "publisher_id")
	var rows [][]interface{}
	for _, k := range keyOrder {
		var dimVals []interface{}
		for _, td := range results {
			if dv, ok := td.dimVals[k]; ok {
				dimVals = dv
				break
			}
		}
		// Resolved base values across tables for this key.
		cvals := map[string]float64{}
		for t, td := range results {
			if mv, ok := td.vals[k]; ok {
				for m, v := range mv {
					cvals[t+"\x00"+m] = v
				}
			}
		}
		pub := pubFilter
		if pubDim && pubIdx >= 0 && pubIdx < len(dimVals) {
			pub = fmt.Sprint(dimVals[pubIdx])
		}
		cc := computeCtx{vals: cvals, publisher: pub, net: e.net}

		row := append([]interface{}{}, dimVals...)
		for _, m := range params.Metrics {
			if d, ok := derivedMetrics[m]; ok {
				if v, ok := d.compute(cc); ok {
					row = append(row, v)
				} else {
					row = append(row, nil) // null → client renders "—"
				}
			} else if v, ok := cvals[primary+"\x00"+m]; ok {
				row = append(row, v)
			} else {
				row = append(row, nil)
			}
		}
		rows = append(rows, row)
	}

	rows = applyOrderLimit(rows, columns, params.OrderBy, params.OrderDir, params.Limit)
	return &analytics.QueryResult{Columns: columns, Rows: rows}, nil
}

// indexResult buckets a query result by the dimension-key (dim values joined
// with \x00), splitting dimension columns from metric columns.
func indexResult(res *analytics.QueryResult, dims []string) *tableData {
	td := &tableData{dimVals: map[string][]interface{}{}, vals: map[string]map[string]float64{}}
	if res == nil {
		return td
	}
	colIdx := map[string]int{}
	for i, c := range res.Columns {
		colIdx[c] = i
	}
	for _, r := range res.Rows {
		dimVals := make([]interface{}, len(dims))
		keyParts := make([]string, len(dims))
		for i, d := range dims {
			if ci, ok := colIdx[d]; ok && ci < len(r) {
				dimVals[i] = r[ci]
				keyParts[i] = fmt.Sprint(r[ci])
			}
		}
		key := strings.Join(keyParts, "\x00")
		td.dimVals[key] = dimVals
		mv := map[string]float64{}
		for c, ci := range colIdx {
			if contains(dims, c) || ci >= len(r) {
				continue
			}
			if f, ok := toFloat(r[ci]); ok {
				mv[c] = f
			}
		}
		td.vals[key] = mv
		td.order = append(td.order, key)
	}
	return td
}

func applyOrderLimit(rows [][]interface{}, columns []string, orderBy, orderDir string, limit int) [][]interface{} {
	if orderBy != "" {
		if ci := indexOf(columns, orderBy); ci >= 0 {
			sort.SliceStable(rows, func(i, j int) bool {
				a, _ := toFloat(rows[i][ci])
				b, _ := toFloat(rows[j][ci])
				if orderDir == "desc" {
					return a > b
				}
				return a < b
			})
		}
	}
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows
}

func toFloat(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint:
		return float64(n), true
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	default:
		return 0, false
	}
}

func contains(ss []string, s string) bool { return indexOf(ss, s) >= 0 }

func indexOf(ss []string, s string) int {
	for i, v := range ss {
		if v == s {
			return i
		}
	}
	return -1
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
