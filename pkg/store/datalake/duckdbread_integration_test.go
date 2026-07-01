//go:build duckdb

// Full cold-path integration test: write Parquet+Delta to Minio via ObjectStore,
// compact, then read it back with DuckDB over delta_scan. Requires a live Minio
// and network (extensions download on first INSTALL):
//
//	MINIO_ENDPOINT=127.0.0.1:9000 go test -tags duckdb -run TestParquetReader ./pkg/store/datalake/...
//
// Locally the Tiltfile forwards Minio to 127.0.0.1:9000.
package datalake

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	objs3 "github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/s3"
)

func TestParquetReader_DeltaScanRoundTrip(t *testing.T) {
	endpoint := os.Getenv("MINIO_ENDPOINT")
	if endpoint == "" {
		t.Skip("set MINIO_ENDPOINT to run the DuckDB-over-Parquet integration test")
	}
	ctx := context.Background()
	bucket := "adtech-datalake-test"

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
	lake := NewObjectStore(obj, bucket, logger.New("dl-it"))
	table := "impressions_it_" + time.Now().Format("150405")

	// Three small writes → three Parquet files, then compact to one.
	for i := 0; i < 3; i++ {
		rec := []Record{{"campaign_id": "c1", "impressions": int64(1), "revenue": 1.0, "viewable": true, "geo": "GBR", "timestamp": time.Now().UTC()}}
		if err := lake.Write(ctx, table, rec, testSchema); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if _, err := lake.Compact(ctx, table); err != nil {
		t.Fatalf("compact: %v", err)
	}

	// DuckDB reads it back over delta_scan — must see exactly 3 rows (compaction
	// tombstones must be skipped, not double-counted).
	reader, err := NewParquetReader(S3Config{
		Endpoint: endpoint, AccessKey: "adtech", SecretKey: "adtech-local-dev", UseSSL: false,
	}, bucket)
	if err != nil {
		t.Fatalf("duckdb reader: %v", err)
	}
	defer reader.Close()

	n, err := reader.CountDeltaScan(ctx, table)
	if err != nil {
		t.Fatalf("count delta_scan: %v", err)
	}
	if n != 3 {
		t.Fatalf("delta_scan count = %d, want 3 (tombstoned parts skipped)", n)
	}
}
