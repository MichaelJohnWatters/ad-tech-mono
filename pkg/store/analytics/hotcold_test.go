package analytics

import (
	"context"
	"testing"
	"time"
)

// fakeCold is an in-memory ColdReader: it records the params it was asked and
// returns a canned result. Lets us assert the tenant filter is carried to cold
// and that boundary splitting queries the right side.
type fakeCold struct {
	lastParams QueryParams
	result     *QueryResult
	err        error
}

func (f *fakeCold) Query(_ context.Context, p QueryParams) (*QueryResult, error) {
	f.lastParams = p
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

func floatCol(t *testing.T, res *QueryResult, key string, col string) float64 {
	t.Helper()
	ci, ki := -1, -1
	for i, c := range res.Columns {
		if c == col {
			ci = i
		}
	}
	// single-row (no dim) case
	if key == "" {
		if len(res.Rows) == 0 {
			t.Fatalf("no rows: %+v", res)
		}
		return toFloatVal(res.Rows[0][ci])
	}
	// find the dim column (first non-metric)
	for i, c := range res.Columns {
		if c != col {
			ki = i
			break
		}
	}
	for _, r := range res.Rows {
		if toStringKey(r[ki]) == key {
			return toFloatVal(r[ci])
		}
	}
	t.Fatalf("key %q not found in %+v", key, res)
	return 0
}

func newTiered(cold ColdReader, hotWindow time.Duration, now time.Time) (*TieredStore, *MemoryStore) {
	hot := NewMemory()
	ts := NewTieredStore(hot, cold, hotWindow, nil)
	ts.now = func() time.Time { return now }
	return ts, hot
}

// TestTiered_HotOnly_NoColdCall: a query fully inside the hot window must not
// touch cold at all.
func TestTiered_HotOnly_NoColdCall(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	cold := &fakeCold{result: &QueryResult{Columns: []string{"count"}, Rows: [][]interface{}{{999.0}}}}
	ts, hot := newTiered(cold, 7*24*time.Hour, now)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_ = hot.InsertImpression(ctx, &ImpressionEvent{PublisherID: "pubA", Timestamp: now.Add(-time.Hour)})
	}
	res, err := ts.Query(ctx, QueryParams{Table: "impressions", Metrics: []string{"count"}, TimeFrom: now.Add(-2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if got := floatCol(t, res, "", "count"); got != 3 {
		t.Errorf("hot count = %v, want 3", got)
	}
	if cold.result != nil && cold.lastParams.Table != "" {
		t.Error("cold was queried for a hot-only range")
	}
}

// TestTiered_ColdOnly: a query entirely before the boundary is served from cold,
// carrying the tenant filter.
func TestTiered_ColdOnly(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	cold := &fakeCold{result: &QueryResult{Columns: []string{"count"}, Rows: [][]interface{}{{500.0}}}}
	ts, _ := newTiered(cold, 7*24*time.Hour, now)
	res, err := ts.Query(context.Background(), QueryParams{
		Table: "impressions", Metrics: []string{"count"},
		Filters:  map[string]string{"publisher_id": "pubA"},
		TimeFrom: now.Add(-30 * 24 * time.Hour), TimeTo: now.Add(-14 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := floatCol(t, res, "", "count"); got != 500 {
		t.Errorf("cold count = %v, want 500", got)
	}
	if cold.lastParams.Filters["publisher_id"] != "pubA" {
		t.Errorf("tenant filter not carried to cold: %+v", cold.lastParams.Filters)
	}
}

// TestTiered_Spanning_MergesAdditive: a range crossing the boundary sums hot +
// cold per dimension key, and both sub-queries carry the tenant filter.
func TestTiered_Spanning_MergesAdditive(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	// Cold returns two placements; hot will add its own recent rows.
	cold := &fakeCold{result: &QueryResult{
		Columns: []string{"placement_id", "count", "sum_cost"},
		Rows: [][]interface{}{
			{"pl1", int64(100), 1.0},
			{"pl2", int64(40), 0.4},
		},
	}}
	ts, hot := newTiered(cold, 7*24*time.Hour, now)
	ctx := context.Background()
	// Hot rows within the window for pl1 only.
	for i := 0; i < 10; i++ {
		_ = hot.InsertImpression(ctx, &ImpressionEvent{PublisherID: "pubA", PlacementID: "pl1", ClearingPriceUSD: 0.01, Timestamp: now.Add(-time.Hour)})
	}
	res, err := ts.Query(ctx, QueryParams{
		Table: "impressions", Metrics: []string{"count", "sum_cost"}, Dimensions: []string{"placement_id"},
		Filters:  map[string]string{"publisher_id": "pubA"},
		TimeFrom: now.Add(-30 * 24 * time.Hour), TimeTo: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	// pl1 = 100 (cold) + 10 (hot) = 110; pl2 = 40 (cold only).
	if got := floatCol(t, res, "pl1", "count"); got != 110 {
		t.Errorf("pl1 count = %v, want 110 (100 cold + 10 hot)", got)
	}
	if got := floatCol(t, res, "pl2", "count"); got != 40 {
		t.Errorf("pl2 count = %v, want 40 (cold only)", got)
	}
	// Both sub-queries must carry the tenant filter (isolation invariant).
	if cold.lastParams.Filters["publisher_id"] != "pubA" {
		t.Errorf("tenant filter dropped on cold: %+v", cold.lastParams.Filters)
	}
	// Cold's upper bound must be the boundary (exclusive), not the caller's `to`.
	boundary := now.Add(-7 * 24 * time.Hour)
	if !cold.lastParams.TimeTo.Before(boundary.Add(time.Nanosecond)) || cold.lastParams.TimeTo.After(boundary) {
		t.Errorf("cold TimeTo = %v, want just under boundary %v", cold.lastParams.TimeTo, boundary)
	}
}

// TestTiered_ColdFailure_DegradesToHot: if cold errors, the hot half is still
// returned (best-effort), never a hard failure.
func TestTiered_ColdFailure_DegradesToHot(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	cold := &fakeCold{err: context.DeadlineExceeded}
	ts, hot := newTiered(cold, 7*24*time.Hour, now)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_ = hot.InsertImpression(ctx, &ImpressionEvent{PublisherID: "pubA", Timestamp: now.Add(-time.Hour)})
	}
	res, err := ts.Query(ctx, QueryParams{
		Table: "impressions", Metrics: []string{"count"},
		TimeFrom: now.Add(-30 * 24 * time.Hour), TimeTo: now,
	})
	if err != nil {
		t.Fatalf("spanning query should degrade, not error: %v", err)
	}
	if got := floatCol(t, res, "", "count"); got != 5 {
		t.Errorf("degraded count = %v, want 5 (hot half)", got)
	}
}

// TestTiered_NilCold_PassesThrough: with no cold reader every read is hot.
func TestTiered_NilCold_PassesThrough(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	ts, hot := newTiered(nil, 7*24*time.Hour, now)
	ctx := context.Background()
	for i := 0; i < 7; i++ {
		_ = hot.InsertImpression(ctx, &ImpressionEvent{PublisherID: "pubA", Timestamp: now.Add(-100 * 24 * time.Hour)})
	}
	// Range fully in cold territory, but cold is nil → hot answers anyway.
	res, err := ts.Query(ctx, QueryParams{Table: "impressions", Metrics: []string{"count"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := floatCol(t, res, "", "count"); got != 7 {
		t.Errorf("hot-only count = %v, want 7", got)
	}
}
