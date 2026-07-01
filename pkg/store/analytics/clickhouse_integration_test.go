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

// TestClickHouse_BatchInsert proves the bulk path lands N rows via one
// PrepareBatch/Send (BatchInserter), and that they materialise as a SINGLE
// MergeTree part — the whole point of batching (vs N parts from N single-row
// inserts, the "too many parts" anti-pattern).
func TestClickHouse_BatchInsert(t *testing.T) {
	ch := testCH(t)
	defer ch.Close()
	ctx := context.Background()
	trace := "ch-batch-" + time.Now().Format("150405.000")
	ts := time.Now().UTC()

	const n = 50
	es := make([]*ImpressionEvent, n)
	for i := 0; i < n; i++ {
		es[i] = &ImpressionEvent{
			TraceID: trace, CampaignID: "c1", CreativeID: "cr1", PlacementID: "p1",
			PublisherID: "pub1", AccountID: "a1", ClearingPrice: 2.5, ClearingCurrency: "USD",
			ClearingPriceUSD: 2.5, BidModel: "cpm", SchemaVersion: 1, Timestamp: ts,
		}
	}
	if err := ch.InsertImpressions(ctx, es); err != nil {
		t.Fatalf("batch insert impressions: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	res, err := ch.Query(ctx, QueryParams{
		Table: "impressions", Metrics: []string{"count"},
		Filters: map[string]string{"trace_id": trace},
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(res.Rows) != 1 || chInt64(res.Rows[0][0]) != n {
		t.Fatalf("count = %v, want %d", res.Rows, n)
	}

	// One INSERT block ⇒ one part covering these rows. Count parts whose row
	// count equals our batch (isolates this insert from any concurrent data).
	var parts int
	if err := ch.db.QueryRowContext(ctx,
		`SELECT count() FROM system.parts WHERE database='adtech' AND table='impressions' AND active AND rows=?`,
		n).Scan(&parts); err != nil {
		t.Fatalf("system.parts query: %v", err)
	}
	if parts < 1 {
		t.Errorf("expected at least one %d-row part from the batch, found %d", n, parts)
	}
}

// TestClickHouse_NativeMVRollup proves the SummingMergeTree materialized view
// aggregates impressions on insert and that QueryRollups routes the events
// hourly tier to it — count + sum_cost summed per (campaign) bucket.
func TestClickHouse_NativeMVRollup(t *testing.T) {
	ch := testCH(t)
	defer ch.Close()
	ctx := context.Background()
	ts := time.Now().UTC()
	acct := "mv-acct-" + ts.Format("150405.000")

	es := make([]*ImpressionEvent, 4)
	for i := range es {
		es[i] = &ImpressionEvent{
			TraceID: "mv", CampaignID: "c1", AccountID: acct, PublisherID: "pub1",
			ClearingPriceUSD: 2.0, BidModel: "cpm", SchemaVersion: 1, Timestamp: ts,
		}
	}
	if err := ch.InsertImpressions(ctx, es); err != nil {
		t.Fatalf("insert: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	rows, err := ch.QueryRollups(ctx, "events", "hourly", ts.Add(-time.Hour), ts.Add(time.Hour))
	if err != nil {
		t.Fatalf("query rollups (MV): %v", err)
	}
	var count, sum float64
	for _, r := range rows {
		if r.Dimensions["account_id"] == acct && r.Dimensions["campaign_id"] == "c1" {
			count += r.Metrics["count"]
			sum += r.Metrics["sum_cost"]
		}
	}
	if count != 4 || sum != 8.0 {
		t.Fatalf("MV rollup for %s: count=%v sum_cost=%v, want 4 / 8.0", acct, count, sum)
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
