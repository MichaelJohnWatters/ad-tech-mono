package analytics

import (
	"fmt"
	"strings"
)

// BuildQuery constructs a SQL query from QueryParams.
// Used by DuckDB and ClickHouse implementations.
func BuildQuery(params QueryParams) (string, []interface{}) {
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
			selectParts = append(selectParts, "COUNT(*) AS count")
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
	b.WriteString(params.Table)

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
