package analytics

import (
	"context"
	"fmt"
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

func newHotCold(cold ColdReader, hotWindow time.Duration, now time.Time) (*HotColdStore, *MemoryStore) {
	hot := NewMemory()
	ts := NewHotColdStore(hot, cold, hotWindow, nil)
	ts.now = func() time.Time { return now }
	return ts, hot
}

// TestHotCold_HotOnly_NoColdCall: a query fully inside the hot window must not
// touch cold at all.
func TestHotCold_HotOnly_NoColdCall(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	cold := &fakeCold{result: &QueryResult{Columns: []string{"count"}, Rows: [][]interface{}{{999.0}}}}
	ts, hot := newHotCold(cold, 7*24*time.Hour, now)
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

// TestHotCold_ColdOnly: a query entirely before the boundary is served from cold,
// carrying the tenant filter.
func TestHotCold_ColdOnly(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	cold := &fakeCold{result: &QueryResult{Columns: []string{"count"}, Rows: [][]interface{}{{500.0}}}}
	ts, _ := newHotCold(cold, 7*24*time.Hour, now)
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

// TestHotCold_Spanning_MergesAdditive: a range crossing the boundary sums hot +
// cold per dimension key, and both sub-queries carry the tenant filter.
func TestHotCold_Spanning_MergesAdditive(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	// Cold returns two placements; hot will add its own recent rows.
	cold := &fakeCold{result: &QueryResult{
		Columns: []string{"placement_id", "count", "sum_cost"},
		Rows: [][]interface{}{
			{"pl1", int64(100), 1.0},
			{"pl2", int64(40), 0.4},
		},
	}}
	ts, hot := newHotCold(cold, 7*24*time.Hour, now)
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

// TestHotCold_ColdFailure_DegradesToHot: if cold errors, the hot half is still
// returned (best-effort), never a hard failure.
func TestHotCold_ColdFailure_DegradesToHot(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	cold := &fakeCold{err: context.DeadlineExceeded}
	ts, hot := newHotCold(cold, 7*24*time.Hour, now)
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

// TestHotCold_NilCold_PassesThrough: with no cold reader every read is hot.
func TestHotCold_NilCold_PassesThrough(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	ts, hot := newHotCold(nil, 7*24*time.Hour, now)
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

// recordingHot wraps a Store and records the params of the last Query — lets
// us assert a hot-only table's query reaches hot with the ORIGINAL params
// (no boundary clamp, no split).
type recordingHot struct {
	Store
	lastParams QueryParams
	result     *QueryResult
}

func (r *recordingHot) Query(_ context.Context, p QueryParams) (*QueryResult, error) {
	r.lastParams = p
	return r.result, nil
}

// TestHotCold_MediaEvents_SpanningMergesQuartiles: media_events is exported to
// the cold lake, so its reads route hot+cold like impressions. A spanning query
// must merge the countIf quartile metrics additively and carry the tenant
// filter to cold (the cold s3() reader is ClickHouse too, so countIf is native
// on both sides).
func TestHotCold_MediaEvents_SpanningMergesQuartiles(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	cold := &fakeCold{result: &QueryResult{
		Columns: []string{"media_starts", "media_completes"},
		Rows:    [][]interface{}{{int64(10), int64(5)}},
	}}
	ts, hot := newHotCold(cold, 7*24*time.Hour, now)
	ctx := context.Background()
	// Hot half: 2 starts + 1 complete inside the hot window.
	for _, et := range []string{"start", "complete", "start"} {
		_ = hot.InsertMediaEvent(ctx, &MediaEvent{
			Channel: "video", EventType: et, AccountID: "acctA", Timestamp: now.Add(-time.Hour),
		})
	}
	res, err := ts.Query(ctx, QueryParams{
		Table: "media_events", Metrics: []string{"media_starts", "media_completes"},
		Filters:  map[string]string{"account_id": "acctA"},
		TimeFrom: now.Add(-30 * 24 * time.Hour), TimeTo: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := floatCol(t, res, "", "media_starts"); got != 12 {
		t.Errorf("media_starts = %v, want 12 (10 cold + 2 hot)", got)
	}
	if got := floatCol(t, res, "", "media_completes"); got != 6 {
		t.Errorf("media_completes = %v, want 6 (5 cold + 1 hot)", got)
	}
	if cold.lastParams.Table != "media_events" {
		t.Errorf("cold not queried for media_events: %+v", cold.lastParams)
	}
	if cold.lastParams.Filters["account_id"] != "acctA" {
		t.Errorf("tenant filter dropped on cold: %+v", cold.lastParams.Filters)
	}
	if res.Approximate != "" {
		t.Errorf("additive quartile merge wrongly stamped approximate: %q", res.Approximate)
	}
}

// TestHotCold_HotOnlyTables_ServedEntirelyHot: the observability spines are
// hot-only BY DESIGN (never exported) — a cold-range query must go to hot with
// the caller's original params, never touch cold, and never carry a degraded/
// approximate stamp.
func TestHotCold_HotOnlyTables_ServedEntirelyHot(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	from := now.Add(-60 * 24 * time.Hour) // entirely before the boundary
	for name := range hotOnlyTables {
		cold := &fakeCold{result: &QueryResult{Columns: []string{"count"}, Rows: [][]interface{}{{999.0}}}}
		hot := &recordingHot{Store: NewMemory(), result: &QueryResult{Columns: []string{"count"}, Rows: [][]interface{}{{3.0}}}}
		ts := NewHotColdStore(hot, cold, 7*24*time.Hour, nil)
		ts.now = func() time.Time { return now }
		res, err := ts.Query(context.Background(), QueryParams{
			Table: name, Metrics: []string{"count"}, TimeFrom: from, TimeTo: now,
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if cold.lastParams.Table != "" {
			t.Errorf("%s: cold was queried for a hot-only table", name)
		}
		if !hot.lastParams.TimeFrom.Equal(from) {
			t.Errorf("%s: hot TimeFrom = %v, want original %v (no boundary clamp)", name, hot.lastParams.TimeFrom, from)
		}
		if res.Approximate != "" {
			t.Errorf("%s: hot-only answer wrongly stamped approximate: %q", name, res.Approximate)
		}
	}
}

// TestExportRoutingConsistency pins the export↔routing contract: media_events
// is exported (quartile history outlives the hot TTL) and therefore NOT
// hot-only, while the six observability spines stay hot-only by design.
func TestExportRoutingConsistency(t *testing.T) {
	exported := map[string]bool{}
	for _, et := range exportTables {
		exported[et.name] = true
	}
	if !exported["media_events"] {
		t.Error("media_events missing from exportTables — quartile history would vanish at the hot TTL")
	}
	if hotOnlyTables["media_events"] {
		t.Error("media_events is exported but still routed hot-only")
	}
	for _, name := range []string{
		"serve_no_fills", "freq_cap_blocks", "render_failures",
		"campaign_state_changes", "budget_depletions", "tracker_rejections",
	} {
		if exported[name] {
			t.Errorf("%s: observability table unexpectedly exported (hot-only by design)", name)
		}
		if !hotOnlyTables[name] {
			t.Errorf("%s: observability table not routed hot-only", name)
		}
	}
}

// A failing cold store must not fail the query — but the degradation must
// ride the RESPONSE (Approximate), not just a server log: a missing lake
// once turned deep-history queries into confidently wrong numbers.
func TestHotCold_ColdFailureIsFlaggedApproximate(t *testing.T) {
	now := time.Now()
	cold := &fakeCold{err: fmt.Errorf("bucket does not exist")}
	hc, _ := newHotCold(cold, 15*time.Minute, now)
	res, err := hc.Query(context.Background(), QueryParams{
		Table:    "impressions",
		Metrics:  []string{"count"},
		TimeFrom: now.Add(-24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("query should degrade, not fail: %v", err)
	}
	if res.Approximate == "" {
		t.Fatal("degraded answer not flagged: Approximate is empty")
	}
}
