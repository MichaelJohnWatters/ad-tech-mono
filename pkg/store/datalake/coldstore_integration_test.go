//go:build duckdb

// Cold-tier integration test: write a known impressions fixture to Minio as
// Parquet+Delta, then read it back THROUGH ColdStore (the analytics.ColdReader
// adapter) — proving BuildQueryFrom → delta_scan produces the right aggregates
// AND that the tenant filter is applied on the cold path (no cross-tenant leak).
//
//	MINIO_ENDPOINT=127.0.0.1:9000 go test -tags duckdb -run TestColdStore ./pkg/store/datalake/...
//
// Locally the Tiltfile / a port-forward exposes Minio on 127.0.0.1:9000.
package datalake

import (
	"context"
	"math"
	"os"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
	objs3 "github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/s3"
)

func TestColdStore_QueryOverLake(t *testing.T) {
	endpoint := os.Getenv("MINIO_ENDPOINT")
	if endpoint == "" {
		t.Skip("set MINIO_ENDPOINT to run the ColdStore-over-Parquet integration test")
	}
	ctx := context.Background()
	// Fresh bucket per run so the fixture is deterministic (canonical table name
	// is required by ColdStore's allowlist, so isolation comes from the bucket).
	bucket := "adtech-coldstore-it-" + time.Now().Format("150405")

	obj, err := objs3.New(objs3.Config{
		Endpoint: endpoint, AccessKey: "adtech", SecretKey: "adtech-local-dev",
		Region: "us-east-1", UseSSL: false,
	})
	if err != nil {
		t.Fatalf("minio connect: %v", err)
	}
	if err := obj.EnsureBucket(ctx, bucket); err != nil {
		t.Fatalf("ensure bucket: %v", err)
	}
	lake := NewObjectStore(obj, bucket, logger.New("cold-it"))

	schema := Schema{Version: 1, Columns: []Column{
		{Name: "publisher_id", Type: "string", Nullable: true},
		{Name: "placement_id", Type: "string", Nullable: true},
		{Name: "clearing_price_usd", Type: "float64", Nullable: true},
		{Name: "timestamp", Type: "timestamp", Nullable: true},
	}}
	now := time.Now().UTC()
	rec := func(pub, pl string, price float64) Record {
		return Record{"publisher_id": pub, "placement_id": pl, "clearing_price_usd": price, "timestamp": now}
	}
	// pubA: 5×pl1 @0.02 (=0.10), 3×pl2 @0.10 (=0.30). pubB: 4×pl1 (other tenant).
	var recs []Record
	for i := 0; i < 5; i++ {
		recs = append(recs, rec("pubA", "pl1", 0.02))
	}
	for i := 0; i < 3; i++ {
		recs = append(recs, rec("pubA", "pl2", 0.10))
	}
	for i := 0; i < 4; i++ {
		recs = append(recs, rec("pubB", "pl1", 0.05))
	}
	if err := lake.Write(ctx, "impressions", recs, schema); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	reader, err := NewParquetReader(S3Config{
		Endpoint: endpoint, AccessKey: "adtech", SecretKey: "adtech-local-dev", UseSSL: false,
	}, bucket)
	if err != nil {
		t.Fatalf("duckdb reader: %v", err)
	}
	cold := NewColdStore(reader)
	defer cold.Close()

	// count + sum_cost grouped by placement_id, scoped to pubA.
	res, err := cold.Query(ctx, analytics.QueryParams{
		Table:      "impressions",
		Metrics:    []string{"count", "sum_cost"},
		Dimensions: []string{"placement_id"},
		Filters:    map[string]string{"publisher_id": "pubA"},
	})
	if err != nil {
		t.Fatalf("cold query: %v", err)
	}

	// Columns must be dimensions then metrics (matching the hot store).
	if got := res.Columns; len(got) != 3 || got[0] != "placement_id" || got[1] != "count" || got[2] != "sum_cost" {
		t.Fatalf("columns = %v, want [placement_id count sum_cost]", got)
	}

	type row struct {
		count float64
		cost  float64
	}
	byPl := map[string]row{}
	var totalCount float64
	pi, ci, si := 0, 1, 2
	for _, r := range res.Rows {
		pl, _ := r[pi].(string)
		c := numOf(r[ci])
		s := numOf(r[si])
		byPl[pl] = row{c, s}
		totalCount += c
	}

	// Tenant isolation: pubB's 4 rows must NOT appear → total pubA count = 8.
	if totalCount != 8 {
		t.Errorf("pubA total count = %v, want 8 (pubB leaked into cold read?)", totalCount)
	}
	if r := byPl["pl1"]; r.count != 5 || math.Abs(r.cost-0.10) > 1e-6 {
		t.Errorf("pl1 = %+v, want {count:5 cost:0.10}", r)
	}
	if r := byPl["pl2"]; r.count != 3 || math.Abs(r.cost-0.30) > 1e-6 {
		t.Errorf("pl2 = %+v, want {count:3 cost:0.30}", r)
	}
}

// numOf coerces a DuckDB scan value (int64 count, float64 sum) to float64.
func numOf(v interface{}) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int64:
		return float64(n)
	case int32:
		return float64(n)
	case int:
		return float64(n)
	default:
		return 0
	}
}
