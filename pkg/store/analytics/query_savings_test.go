package analytics

import (
	"strings"
	"testing"
)

// sum_savings is valid ONLY on auction_shades (the only table with a savings_usd
// column). Everywhere else it must resolve to a harmless 0 — same contract as
// sum_cost/sum_revenue on tables that lack their column.
func TestBuildQuery_SumSavings(t *testing.T) {
	t.Run("auction_shades sums savings_usd", func(t *testing.T) {
		sql, _ := BuildQuery(QueryParams{
			Table:   "auction_shades",
			Metrics: []string{"sum_savings"},
			Filters: map[string]string{"account_id": "acct-1"},
		})
		if !strings.Contains(sql, "SUM(savings_usd) AS sum_savings") {
			t.Fatalf("expected SUM(savings_usd), got: %s", sql)
		}
		if !strings.Contains(sql, "FROM auction_shades") {
			t.Fatalf("expected FROM auction_shades, got: %s", sql)
		}
		if !strings.Contains(sql, "account_id = ?") {
			t.Fatalf("expected account_id filter, got: %s", sql)
		}
	})

	t.Run("other tables resolve savings to 0", func(t *testing.T) {
		sql, _ := BuildQuery(QueryParams{
			Table:   "impressions",
			Metrics: []string{"sum_savings"},
		})
		if !strings.Contains(sql, "0 AS sum_savings") {
			t.Fatalf("expected 0 AS sum_savings on impressions, got: %s", sql)
		}
	})
}
