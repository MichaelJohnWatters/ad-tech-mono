//go:build duckdb

// Cold-path compaction test: write three Parquet parts to Minio via ObjectStore,
// compact them into one (which tombstones the originals in our Delta log), then
// read back through ColdStore — the count must be 3, proving the cold reader
// honours the active-file set and does NOT double-count compacted-away parts.
// Requires a live Minio and network (DuckDB extensions download on first
// INSTALL):
//
//	MINIO_ENDPOINT=127.0.0.1:9000 go test -tags duckdb -run TestColdStore ./pkg/store/datalake/...
//
// NB: our lake uses a home-grown _delta_log (one custom transaction JSON per
// file), NOT the real Delta protocol — so DuckDB's delta_scan() cannot read it.
// ColdStore instead resolves the active parquet files from that log and reads
// them with read_parquet([...]); this test guards that tombstone-correctness.
// Locally the Tiltfile / a port-forward exposes Minio on 127.0.0.1:9000.
package datalake

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
	objs3 "github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/s3"
)

func TestColdStore_CompactionTombstoneSkip(t *testing.T) {
	endpoint := os.Getenv("MINIO_ENDPOINT")
	if endpoint == "" {
		t.Skip("set MINIO_ENDPOINT to run the ColdStore compaction integration test")
	}
	ctx := context.Background()
	bucket := "adtech-coldstore-compact-it-" + time.Now().Format("150405")

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
	lake := NewObjectStore(obj, bucket, logger.New("cold-compact-it"))

	// Three small writes → three Parquet files, then compact to one.
	for i := 0; i < 3; i++ {
		rec := []Record{{"campaign_id": "c1", "impressions": int64(1), "revenue": 1.0, "viewable": true, "geo": "GBR", "timestamp": time.Now().UTC()}}
		if err := lake.Write(ctx, "impressions", rec, testSchema); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if _, err := lake.Compact(ctx, "impressions"); err != nil {
		t.Fatalf("compact: %v", err)
	}

	reader, err := NewParquetReader(S3Config{
		Endpoint: endpoint, AccessKey: "adtech", SecretKey: "adtech-local-dev", UseSSL: false,
	}, bucket)
	if err != nil {
		t.Fatalf("duckdb reader: %v", err)
	}
	cold := NewColdStore(reader)
	defer cold.Close()

	res, err := cold.Query(ctx, analytics.QueryParams{Table: "impressions", Metrics: []string{"count"}})
	if err != nil {
		t.Fatalf("cold count query: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("want 1 row, got %d: %+v", len(res.Rows), res)
	}
	if n := numOf(res.Rows[0][0]); n != 3 {
		t.Fatalf("cold count = %v, want 3 (tombstoned parts must be skipped, not double-counted)", n)
	}
}
