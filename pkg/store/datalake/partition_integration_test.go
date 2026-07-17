//go:build duckdb

// Proves the PARTITIONED lake against the standard Delta reader: DuckDB's
// delta_scan() must read hive-layout tables Write/Compact/PurgeRows emit,
// surface the event_date partition column, and keep working through the
// EnsurePartitioned flat→partitioned migration — external-reader
// compatibility is the whole point of writing real Delta metadata.
//
//	MINIO_ENDPOINT=127.0.0.1:9000 go test -tags duckdb -run TestPartitionedDelta ./pkg/store/datalake/...
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

func TestPartitionedDelta_DeltaScanReadsHiveLayout(t *testing.T) {
	endpoint := os.Getenv("MINIO_ENDPOINT")
	if endpoint == "" {
		t.Skip("set MINIO_ENDPOINT to run the partitioned delta_scan test")
	}
	ctx := context.Background()
	bucket := "adtech-partition-it-" + time.Now().Format("150405")

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
	lake := NewObjectStore(obj, bucket, logger.New("partition-it"))

	schema := Schema{
		Version: 1,
		Columns: []Column{
			{Name: "user_id", Type: "string", Nullable: true},
			{Name: "kind", Type: "string", Nullable: true},
			{Name: "observed_at", Type: "timestamp", Nullable: true},
		},
		PartitionBy: "observed_at",
	}
	at := func(d, h int) time.Time { return time.Date(2026, 7, d, h, 0, 0, 0, time.UTC) }
	rec := func(user string, ts time.Time) Record {
		return Record{"user_id": user, "kind": "site_visit", "observed_at": ts}
	}

	// Three commits spanning two days; day 10 gets two files (compactable).
	if err := lake.Write(ctx, "behaviour_signals", []Record{rec("u1", at(10, 9)), rec("u2", at(11, 9))}, schema); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	if err := lake.Write(ctx, "behaviour_signals", []Record{rec("u1", at(10, 15))}, schema); err != nil {
		t.Fatalf("write 2: %v", err)
	}
	if err := lake.Write(ctx, "behaviour_signals", []Record{rec("u3", at(11, 12))}, schema); err != nil {
		t.Fatalf("write 3: %v", err)
	}

	reader, err := NewParquetReader(S3Config{
		Endpoint: endpoint, AccessKey: "adtech", SecretKey: "adtech-local-dev", UseSSL: false,
	}, bucket)
	if err != nil {
		t.Fatalf("duckdb reader: %v", err)
	}
	defer reader.Close()
	uri := fmt.Sprintf("s3://%s/behaviour_signals", bucket)
	scanCount := func(where string) float64 {
		t.Helper()
		q := fmt.Sprintf("SELECT count(*) AS c FROM delta_scan('%s') %s", uri, where)
		rows, err := reader.Query(ctx, q)
		if err != nil {
			t.Fatalf("delta_scan %q: %v", where, err)
		}
		return numOf(rows[0]["c"])
	}

	if n := scanCount(""); n != 4 {
		t.Fatalf("delta_scan total = %v, want 4", n)
	}
	// The partition column must be queryable — this is what standard
	// engines use for partition elimination.
	if n := scanCount("WHERE event_date = DATE '2026-07-10'"); n != 2 {
		t.Errorf("event_date=07-10 count = %v, want 2", n)
	}
	if n := scanCount("WHERE event_date >= DATE '2026-07-11'"); n != 2 {
		t.Errorf("event_date>=07-11 count = %v, want 2", n)
	}

	// Per-partition compact: still 4 rows, same partition query results.
	if _, err := lake.Compact(ctx, "behaviour_signals"); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if n := scanCount(""); n != 4 {
		t.Errorf("post-compact total = %v, want 4", n)
	}
	if n := scanCount("WHERE event_date = DATE '2026-07-10'"); n != 2 {
		t.Errorf("post-compact event_date=07-10 = %v, want 2", n)
	}

	// GDPR purge through the standard reader's eyes.
	if _, err := lake.PurgeRows(ctx, "behaviour_signals", func(r Record) bool { return r["user_id"] == "u1" }); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n := scanCount("WHERE user_id = 'u1'"); n != 0 {
		t.Errorf("post-purge u1 rows visible to delta_scan = %v, want 0", n)
	}
	if n := scanCount(""); n != 2 {
		t.Errorf("post-purge total = %v, want 2", n)
	}
}

func TestPartitionedDelta_MigrationReadableByDeltaScan(t *testing.T) {
	endpoint := os.Getenv("MINIO_ENDPOINT")
	if endpoint == "" {
		t.Skip("set MINIO_ENDPOINT to run the partitioned delta_scan test")
	}
	ctx := context.Background()
	bucket := "adtech-partmig-it-" + time.Now().Format("150405")

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
	lake := NewObjectStore(obj, bucket, logger.New("partmig-it"))

	partitioned := Schema{
		Version: 1,
		Columns: []Column{
			{Name: "user_id", Type: "string", Nullable: true},
			{Name: "observed_at", Type: "timestamp", Nullable: true},
		},
		PartitionBy: "observed_at",
	}
	flat := partitioned
	flat.PartitionBy = ""
	at := func(d int) time.Time { return time.Date(2026, 7, d, 12, 0, 0, 0, time.UTC) }

	// Flat-era history, then the boot migration, then a partitioned-era write.
	if err := lake.Write(ctx, "t", []Record{
		{"user_id": "u1", "observed_at": at(10)},
		{"user_id": "u2", "observed_at": at(11)},
	}, flat); err != nil {
		t.Fatalf("write flat: %v", err)
	}
	if err := lake.EnsurePartitioned(ctx, "t", partitioned); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := lake.Write(ctx, "t", []Record{{"user_id": "u3", "observed_at": at(12)}}, partitioned); err != nil {
		t.Fatalf("write partitioned: %v", err)
	}

	reader, err := NewParquetReader(S3Config{
		Endpoint: endpoint, AccessKey: "adtech", SecretKey: "adtech-local-dev", UseSSL: false,
	}, bucket)
	if err != nil {
		t.Fatalf("duckdb reader: %v", err)
	}
	defer reader.Close()
	uri := fmt.Sprintf("s3://%s/t", bucket)

	rows, err := reader.Query(ctx, fmt.Sprintf("SELECT count(*) AS c FROM delta_scan('%s')", uri))
	if err != nil {
		t.Fatalf("delta_scan after migration: %v", err)
	}
	if n := numOf(rows[0]["c"]); n != 3 {
		t.Fatalf("post-migration total = %v, want 3 (2 migrated + 1 new)", n)
	}
	rows, err = reader.Query(ctx, fmt.Sprintf(
		"SELECT count(*) AS c FROM delta_scan('%s') WHERE event_date >= DATE '2026-07-11'", uri))
	if err != nil {
		t.Fatalf("delta_scan partition filter: %v", err)
	}
	if n := numOf(rows[0]["c"]); n != 2 {
		t.Errorf("event_date>=07-11 = %v, want 2", n)
	}
}
