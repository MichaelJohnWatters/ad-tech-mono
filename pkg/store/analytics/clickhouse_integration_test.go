//go:build clickhouse_integration

// Run against a live ClickHouse:
//
//	CLICKHOUSE_ADDR=127.0.0.1:9010 go test -tags clickhouse_integration ./pkg/store/analytics/...
//
// Locally the Tiltfile forwards the in-cluster clickhouse to 127.0.0.1:9010.
package analytics

import (
	"context"
	"os"
	"testing"
	"time"
)

func testCH(t *testing.T) *ClickHouse {
	t.Helper()
	addr := os.Getenv("CLICKHOUSE_ADDR")
	if addr == "" {
		t.Skip("set CLICKHOUSE_ADDR to run the clickhouse integration test")
	}
	ch, err := NewClickHouse(ClickHouseConfig{
		Addrs:    []string{addr},
		Database: "adtech",
		Username: "adtech",
		Password: "adtech-local-dev",
	})
	if err != nil {
		t.Fatalf("connect clickhouse: %v", err)
	}
	return ch
}

func TestClickHouse_InsertAndQuery(t *testing.T) {
	ch := testCH(t)
	defer ch.Close()
	ctx := context.Background()
	trace := "ch-it-" + time.Now().Format("150405.000")
	ts := time.Now().UTC()

	for i := 0; i < 3; i++ {
		if err := ch.InsertImpression(ctx, &ImpressionEvent{
			TraceID: trace, CampaignID: "c1", CreativeID: "cr1", PlacementID: "p1",
			PublisherID: "pub1", AccountID: "a1", ClearingPrice: 2.5, ClearingCurrency: "USD",
			ClearingPriceUSD: 2.5, SchemaVersion: 1, Timestamp: ts,
		}); err != nil {
			t.Fatalf("insert impression: %v", err)
		}
	}
	// ClickHouse inserts are async-visible; give the part a moment.
	time.Sleep(200 * time.Millisecond)

	res, err := ch.Query(ctx, QueryParams{
		Table: "impressions", Metrics: []string{"count", "sum_cost"},
		Filters: map[string]string{"trace_id": trace},
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(res.Rows))
	}
	if n := chInt64(res.Rows[0][0]); n != 3 {
		t.Errorf("count = %v, want 3", res.Rows[0][0])
	}
}

// chInt64 normalises whatever integer width the ClickHouse driver returns
// for a COUNT(*).
func chInt64(v interface{}) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case uint64:
		return int64(n)
	case int32:
		return int64(n)
	case int:
		return int64(n)
	default:
		return -1
	}
}

func TestClickHouse_RollupsAndSignals(t *testing.T) {
	ch := testCH(t)
	defer ch.Close()
	ctx := context.Background()
	ts := time.Now().UTC()

	if err := ch.InsertRollups(ctx, []RollupRow{{
		Config: "events", Level: "hourly", WindowFrom: ts.Add(-time.Hour), WindowTo: ts,
		Dimensions: map[string]string{"campaign_id": "c1"}, Metrics: map[string]float64{"count": 5},
	}}); err != nil {
		t.Fatalf("insert rollups: %v", err)
	}
	// Operational signal (fire-and-forget) shouldn't panic or error the caller.
	ch.InsertFreqCapBlock(FreqCapBlock{TraceID: "t1", CampaignID: "c1", Timestamp: ts})
}
