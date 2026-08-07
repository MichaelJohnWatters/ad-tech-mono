package analytics

import (
	"fmt"
	"strings"
)

// BuildQuery constructs a SQL query from QueryParams against params.Table.
// Used by the DuckDB and ClickHouse implementations.
func BuildQuery(params QueryParams) (string, []interface{}) {
	return BuildQueryFrom(params, params.Table)
}

// BuildQueryFrom is BuildQuery with an explicit FROM expression. The hot store
// passes the table name; the cold tier passes a read_parquet([...]) expression
// over the active file set so BOTH compute identical aggregates AND apply the
// identical tenant WHERE —
// which keeps the hot/cold boundary merge sound and prevents a filter from being
// dropped on the cold path (a cross-tenant leak). Everything but the FROM target
// is shared.
func BuildQueryFrom(params QueryParams, fromExpr string) (string, []interface{}) {
	var b strings.Builder
	var args []interface{}

	// SELECT clause
	b.WriteString("SELECT ")
	var selectParts []string
	for _, dim := range params.Dimensions {
		switch dim {
		case "day":
			selectParts = append(selectParts, "CAST(timestamp AS DATE) AS day")
		case "hour":
			selectParts = append(selectParts, "DATE_TRUNC('hour', timestamp) AS hour")
		default:
			selectParts = append(selectParts, dim)
		}
	}
	for _, metric := range params.Metrics {
		switch metric {
		case "count":
			if params.Table == "impressions" {
				// DOOH proof-of-play carries impression_qty > 1 (the venue audience
				// per play); every other row is 1, so SUM(impression_qty) is the
				// true delivered-impression count and equals COUNT(*) for all
				// non-DOOH rows. Also feeds the app-side rollup engine (rollup.go
				// computes via store.Query), so rollups stay consistent.
				selectParts = append(selectParts, "SUM(impression_qty) AS count")
			} else {
				selectParts = append(selectParts, "COUNT(*) AS count")
			}
		case "sum_cost":
			selectParts = append(selectParts, "SUM(clearing_price_usd) AS sum_cost")
		case "avg_cost":
			selectParts = append(selectParts, "AVG(clearing_price_usd) AS avg_cost")
		case "sum_revenue":
			selectParts = append(selectParts, "SUM(revenue_usd) AS sum_revenue")
		case "avg_duration_ms":
			selectParts = append(selectParts, "AVG(duration_ms) AS avg_duration_ms")
		case "sum_bids":
			selectParts = append(selectParts, "SUM(num_bids) AS sum_bids")
		case "sum_viewable":
			// views.iab_viewable is 0/1, so SUM == the count of IAB-viewable
			// views — the numerator for viewability_rate.
			selectParts = append(selectParts, "SUM(iab_viewable) AS sum_viewable")
		case "media_starts":
			// media_events quartile counts: countIf is ClickHouse-native, which
			// is fine — media_events is hot-only (not in exportTables, so the
			// cold s3() path never sees these metrics). Denominator for
			// completion_rate.
			selectParts = append(selectParts, "countIf(event_type = 'start') AS media_starts")
		case "media_completes":
			selectParts = append(selectParts, "countIf(event_type = 'complete') AS media_completes")
		default:
			selectParts = append(selectParts, metric)
		}
	}
	if len(selectParts) == 0 {
		selectParts = append(selectParts, "COUNT(*) AS count")
	}
	b.WriteString(strings.Join(selectParts, ", "))

	// FROM
	b.WriteString(" FROM ")
	b.WriteString(fromExpr)

	// WHERE
	var conditions []string
	if !params.TimeFrom.IsZero() {
		conditions = append(conditions, "timestamp >= ?")
		args = append(args, params.TimeFrom)
	}
	if !params.TimeTo.IsZero() {
		conditions = append(conditions, "timestamp <= ?")
		args = append(args, params.TimeTo)
	}
	for col, val := range params.Filters {
		conditions = append(conditions, col+" = ?")
		args = append(args, val)
	}
	if len(conditions) > 0 {
		b.WriteString(" WHERE ")
		b.WriteString(strings.Join(conditions, " AND "))
	}

	// GROUP BY
	if len(params.Dimensions) > 0 {
		b.WriteString(" GROUP BY ")
		var groupParts []string
		for _, dim := range params.Dimensions {
			switch dim {
			case "day":
				groupParts = append(groupParts, "CAST(timestamp AS DATE)")
			case "hour":
				groupParts = append(groupParts, "DATE_TRUNC('hour', timestamp)")
			default:
				groupParts = append(groupParts, dim)
			}
		}
		b.WriteString(strings.Join(groupParts, ", "))
	}

	// ORDER BY
	if params.OrderBy != "" {
		b.WriteString(" ORDER BY ")
		b.WriteString(params.OrderBy)
		if params.OrderDir == "desc" {
			b.WriteString(" DESC")
		}
	}

	// LIMIT
	if params.Limit > 0 {
		b.WriteString(fmt.Sprintf(" LIMIT %d", params.Limit))
	}

	return b.String(), args
}
