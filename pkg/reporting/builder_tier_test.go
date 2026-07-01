package reporting

import (
	"context"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

// seedRollups writes rollup rows into a memory store for the tier tests.
func rr(config, level string, from, to time.Time, dims map[string]string, mets map[string]float64) analytics.RollupRow {
	return analytics.RollupRow{Config: config, Level: level, WindowFrom: from, WindowTo: to, Dimensions: dims, Metrics: mets}
}

// A 3h range selects the hourly tier; the builder re-aggregates hourly rollup
// rows by campaign, summing across windows, and honours the account filter.
func TestBuilder_ReadsHourlyRollups(t *testing.T) {
	store := analytics.NewMemory()
	ctx := context.Background()
	to := time.Now().UTC().Truncate(time.Hour)
	from := to.Add(-3 * time.Hour)
	w1f, w1t := to.Add(-time.Hour), to
	w2f, w2t := to.Add(-2*time.Hour), to.Add(-time.Hour)

	if err := store.InsertRollups(ctx, []analytics.RollupRow{
		rr("events", "hourly", w1f, w1t, map[string]string{"account_id": "acct1", "campaign_id": "c1"}, map[string]float64{"count": 10, "sum_cost": 20}),
		rr("events", "hourly", w1f, w1t, map[string]string{"account_id": "acct1", "campaign_id": "c2"}, map[string]float64{"count": 5, "sum_cost": 8}),
		rr("events", "hourly", w2f, w2t, map[string]string{"account_id": "acct1", "campaign_id": "c1"}, map[string]float64{"count": 3, "sum_cost": 6}),
		rr("events", "hourly", w1f, w1t, map[string]string{"account_id": "acct2", "campaign_id": "c1"}, map[string]float64{"count": 100, "sum_cost": 999}),
	}); err != nil {
		t.Fatalf("seed rollups: %v", err)
	}

	res, err := NewBuilder(store).
		Table("impressions").Metrics("count", "sum_cost").GroupBy("campaign_id").
		ForAccount("acct1").TimeRange(from, to).AutoTier().Build(ctx)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// Expect two rows (acct2 excluded): c1 summed across windows (13/26), c2 (5/8).
	got := map[string][2]float64{}
	for _, row := range res.Rows {
		got[row[0].(string)] = [2]float64{toF(row[1]), toF(row[2])}
	}
	if len(got) != 2 {
		t.Fatalf("rows = %d (%+v), want 2 (acct2 filtered out)", len(res.Rows), res.Rows)
	}
	if got["c1"] != [2]float64{13, 26} {
		t.Errorf("c1 = %v, want [13 26] (summed across 2 hourly windows)", got["c1"])
	}
	if got["c2"] != [2]float64{5, 8} {
		t.Errorf("c2 = %v, want [5 8]", got["c2"])
	}
}

// A 48h range selects the daily tier; hourly rollups for the same campaign must
// be ignored (no double count across tiers).
func TestBuilder_TierSelectsDailyOverHourly(t *testing.T) {
	store := analytics.NewMemory()
	ctx := context.Background()
	to := time.Now().UTC().Truncate(time.Hour)
	from := to.Add(-48 * time.Hour)

	if err := store.InsertRollups(ctx, []analytics.RollupRow{
		rr("events", "hourly", to.Add(-time.Hour), to, map[string]string{"account_id": "acct1", "campaign_id": "c1"}, map[string]float64{"count": 10}),
		rr("events", "daily", to.Add(-24*time.Hour), to.Add(-23*time.Hour), map[string]string{"account_id": "acct1", "campaign_id": "c1"}, map[string]float64{"count": 7}),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	res, err := NewBuilder(store).
		Table("impressions").Metrics("count").GroupBy("campaign_id").
		ForAccount("acct1").TimeRange(from, to).AutoTier().Build(ctx)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0][0].(string) != "c1" || toF(res.Rows[0][1]) != 7 {
		t.Fatalf("got %+v, want single c1=7 from the daily tier (hourly ignored)", res.Rows)
	}
}

// When no rollups exist, AutoTier falls back to a raw scan — correctness never
// depends on the rollups being present.
func TestBuilder_FallsBackToRawWhenNoRollups(t *testing.T) {
	store := analytics.NewMemory()
	ctx := context.Background()
	now := time.Now().UTC()
	for i := 0; i < 2; i++ {
		if err := store.InsertImpression(ctx, &analytics.ImpressionEvent{
			TraceID: "t", CampaignID: "c1", AccountID: "acct1", ClearingPriceUSD: 1, Timestamp: now,
		}); err != nil {
			t.Fatalf("insert impression: %v", err)
		}
	}

	res, err := NewBuilder(store).
		Table("impressions").Metrics("count").GroupBy("campaign_id").
		ForAccount("acct1").TimeRange(now.Add(-time.Hour), now.Add(time.Hour)).AutoTier().Build(ctx)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// Raw path answers from the 2 impressions.
	if len(res.Rows) != 1 || toF(res.Rows[0][1]) != 2 {
		t.Fatalf("got %+v, want c1 count=2 from raw fallback", res.Rows)
	}
}

// A non-additive metric (avg_*) can't be re-aggregated from rollups, so the
// builder falls back to raw rather than returning a wrong average.
func TestBuilder_NonAdditiveMetricFallsBack(t *testing.T) {
	store := analytics.NewMemory()
	ctx := context.Background()
	to := time.Now().UTC().Truncate(time.Hour)
	from := to.Add(-2 * time.Hour)
	// Seed an auctions rollup that WOULD match on dims but has avg metric.
	if err := store.InsertRollups(ctx, []analytics.RollupRow{
		rr("auctions", "hourly", to.Add(-time.Hour), to, map[string]string{"placement_id": "p1"}, map[string]float64{"avg_duration_ms": 42}),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// avg_duration_ms is non-additive → fall back to raw auctions (empty) → count path.
	res, err := NewBuilder(store).
		Table("auctions").Metrics("avg_duration_ms").GroupBy("placement_id").
		TimeRange(from, to).AutoTier().Build(ctx)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// Raw memory auctions query returns the count/avg shape, not the rollup's 42.
	if len(res.Rows) == 1 && toF(res.Rows[0][len(res.Rows[0])-1]) == 42 {
		t.Errorf("returned the rollup avg (42) — should have fallen back to raw")
	}
}

func toF(v interface{}) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case int:
		return float64(n)
	default:
		return -1
	}
}
