//go:build duckdb

// Proves the migrated write path: ObjectStore.Write now commits a REAL Delta
// transaction log, so DuckDB's delta_scan() — the standard Delta reader that
// could NOT read our old home-grown log — reads the lake directly, across
// multiple commits, including a tenant-filtered aggregate and a timestamp
// column. No hand-built log: this is exactly what Write emits.
//
//	MINIO_ENDPOINT=127.0.0.1:9000 go test -tags duckdb -run TestDeltaLog ./pkg/store/datalake/...
package datalake

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	objs3 "github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/s3"
)

func TestDeltaLog_WriteIsReadableByDeltaScan(t *testing.T) {
	endpoint := os.Getenv("MINIO_ENDPOINT")
	if endpoint == "" {
		t.Skip("set MINIO_ENDPOINT to run the real-Delta-log delta_scan test")
	}
	ctx := context.Background()
	bucket := "adtech-deltalog-it-" + time.Now().Format("150405")

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
	lake := NewObjectStore(obj, bucket, logger.New("deltalog-it"))

	schema := Schema{Version: 1, Columns: []Column{
		{Name: "publisher_id", Type: "string", Nullable: true},
		{Name: "clearing_price_usd", Type: "float64", Nullable: true},
		{Name: "timestamp", Type: "timestamp", Nullable: true},
	}}
	now := time.Now().UTC()
	rec := func(pub string, price float64) Record {
		return Record{"publisher_id": pub, "clearing_price_usd": price, "timestamp": now}
	}

	// Two batches → two commits (v0 with protocol/metaData, v1 a plain add).
	batchA := []Record{rec("pubA", 0.02), rec("pubA", 0.02), rec("pubA", 0.02), rec("pubA", 0.02), rec("pubA", 0.02), rec("pubB", 0.05), rec("pubB", 0.05)}
	batchB := []Record{rec("pubA", 0.10), rec("pubA", 0.10), rec("pubA", 0.10)}
	if err := lake.Write(ctx, "impressions", batchA, schema); err != nil {
		t.Fatalf("write A: %v", err)
	}
	if err := lake.Write(ctx, "impressions", batchB, schema); err != nil {
		t.Fatalf("write B: %v", err)
	}

	// The standard Delta reader must now read what Write emitted — no manual log.
	reader, err := NewParquetReader(S3Config{
		Endpoint: endpoint, AccessKey: "adtech", SecretKey: "adtech-local-dev", UseSSL: false,
	}, bucket)
	if err != nil {
		t.Fatalf("duckdb reader: %v", err)
	}
	defer reader.Close()

	uri := fmt.Sprintf("s3://%s/impressions", bucket)
	rows, err := reader.Query(ctx, fmt.Sprintf("SELECT count(*) AS c FROM delta_scan('%s')", uri))
	if err != nil {
		t.Fatalf("delta_scan count — Write did not emit readable Delta: %v", err)
	}
	if n := numOf(rows[0]["c"]); n != 10 {
		t.Fatalf("delta_scan total = %v, want 10 (both commits)", n)
	}

	rows, err = reader.Query(ctx, fmt.Sprintf(
		"SELECT count(*) AS c, sum(clearing_price_usd) AS s FROM delta_scan('%s') WHERE publisher_id = 'pubA'", uri))
	if err != nil {
		t.Fatalf("delta_scan filtered: %v", err)
	}
	if n := numOf(rows[0]["c"]); n != 8 {
		t.Errorf("pubA count = %v, want 8", n)
	}
	if s := numOf(rows[0]["s"]); s < 0.399 || s > 0.401 {
		t.Errorf("pubA sum_cost = %v, want ~0.40 (5*0.02 + 3*0.10)", s)
	}
}
