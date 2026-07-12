//go:build duckdb

// PROTOTYPE (de-risks the real-Delta-log migration): write our normal Parquet
// parts to Minio, then hand them a REAL Delta transaction log built by
// deltalog.go (protocol/metaData/add across two commits) and confirm DuckDB's
// delta_scan() — the standard Delta reader that fails on our home-grown log —
// reads it correctly, including a tenant-filtered aggregate.
//
//	MINIO_ENDPOINT=127.0.0.1:9000 go test -tags duckdb -run TestDeltaLog_RealFormat ./pkg/store/datalake/...
//
// If this passes, the migration is just: emit this format from ObjectStore.Write
// /Compact instead of the custom Transaction JSON.
package datalake

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	objs3 "github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/s3"
)

func TestDeltaLog_RealFormat_ReadableByDeltaScan(t *testing.T) {
	endpoint := os.Getenv("MINIO_ENDPOINT")
	if endpoint == "" {
		t.Skip("set MINIO_ENDPOINT to run the real-Delta-log delta_scan prototype")
	}
	ctx := context.Background()
	bucket := "adtech-deltalog-proto-" + time.Now().Format("150405")

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
	lake := NewObjectStore(obj, bucket, logger.New("deltalog-proto"))

	schema := Schema{Version: 1, Columns: []Column{
		{Name: "publisher_id", Type: "string", Nullable: true},
		{Name: "clearing_price_usd", Type: "float64", Nullable: true},
		{Name: "timestamp", Type: "timestamp", Nullable: true},
	}}
	now := time.Now().UTC()
	rec := func(pub string, price float64) Record {
		return Record{"publisher_id": pub, "clearing_price_usd": price, "timestamp": now}
	}

	// Two batches → two Parquet parts (our normal encoder), two commits.
	batchA := []Record{rec("pubA", 0.02), rec("pubA", 0.02), rec("pubA", 0.02), rec("pubA", 0.02), rec("pubA", 0.02), rec("pubB", 0.05), rec("pubB", 0.05)}
	batchB := []Record{rec("pubA", 0.10), rec("pubA", 0.10), rec("pubA", 0.10)}
	if err := lake.Write(ctx, "impressions", batchA, schema); err != nil {
		t.Fatalf("write A: %v", err)
	}
	if err := lake.Write(ctx, "impressions", batchB, schema); err != nil {
		t.Fatalf("write B: %v", err)
	}

	// Read our own log to get the parquet paths + sizes, then OVERWRITE the log
	// files with a real Delta commit log referencing the same parquet parts.
	txns, err := lake.Log(ctx, "impressions")
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	if len(txns) != 2 {
		t.Fatalf("want 2 txns, got %d", len(txns))
	}
	nowMs := now.UnixMilli()
	// add.path is RELATIVE to the table root (strip the "impressions/" prefix).
	relPath := func(p string) string { return strings.TrimPrefix(p, "impressions/") }

	add0 := deltaAdd{Path: relPath(txns[0].Path), PartitionValues: map[string]string{}, Size: txns[0].ByteSize, ModificationTime: nowMs, DataChange: true}
	commit0, err := buildDeltaCommit0("proto-table-uuid", schema, add0, nowMs)
	if err != nil {
		t.Fatalf("build commit0: %v", err)
	}
	add1 := deltaAdd{Path: relPath(txns[1].Path), PartitionValues: map[string]string{}, Size: txns[1].ByteSize, ModificationTime: nowMs, DataChange: true}
	commit1, err := buildDeltaCommitAdd(add1)
	if err != nil {
		t.Fatalf("build commit1: %v", err)
	}
	putLog := func(v int, body []byte) {
		key := fmt.Sprintf("impressions/_delta_log/%020d.json", v)
		if err := obj.Put(ctx, bucket, key, bytes.NewReader(body), int64(len(body)), "application/json"); err != nil {
			t.Fatalf("put log v%d: %v", v, err)
		}
	}
	putLog(0, commit0)
	putLog(1, commit1)

	// Now the standard Delta reader must read it.
	reader, err := NewParquetReader(S3Config{
		Endpoint: endpoint, AccessKey: "adtech", SecretKey: "adtech-local-dev", UseSSL: false,
	}, bucket)
	if err != nil {
		t.Fatalf("duckdb reader: %v", err)
	}
	defer reader.Close()

	uri := fmt.Sprintf("s3://%s/impressions", bucket)

	// Total rows across both commits.
	rows, err := reader.Query(ctx, fmt.Sprintf("SELECT count(*) AS c FROM delta_scan('%s')", uri))
	if err != nil {
		t.Fatalf("delta_scan count failed — real Delta log not readable: %v", err)
	}
	if n := asInt(rows[0]["c"]); n != 10 {
		t.Fatalf("delta_scan total = %d, want 10", n)
	}

	// Tenant-filtered aggregate straight through delta_scan.
	rows, err = reader.Query(ctx, fmt.Sprintf(
		"SELECT count(*) AS c, sum(clearing_price_usd) AS s FROM delta_scan('%s') WHERE publisher_id = 'pubA'", uri))
	if err != nil {
		t.Fatalf("delta_scan filtered query: %v", err)
	}
	if n := asInt(rows[0]["c"]); n != 8 {
		t.Errorf("pubA count = %d, want 8", n)
	}
	if s := asFloat(rows[0]["s"]); s < 0.399 || s > 0.401 {
		t.Errorf("pubA sum_cost = %v, want ~0.40 (5*0.02 + 3*0.10)", s)
	}
}

func asInt(v interface{}) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int32:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return -1
	}
}

func asFloat(v interface{}) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int64:
		return float64(n)
	default:
		return 0
	}
}
