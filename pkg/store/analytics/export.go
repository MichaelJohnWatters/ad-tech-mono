package analytics

import (
	"context"
	"fmt"
	"time"
)

// ParquetExporter snapshots ClickHouse tables to Parquet on S3 (ADR 0006 phase
// 4). Implemented by *ClickHouse; the reporting export endpoint type-asserts it
// (unwrapping a HotColdStore first).
type ParquetExporter interface {
	ExportHourToParquet(ctx context.Context, cfg ExportConfig, hour time.Time) (map[string]int64, error)
	ExportSnapshot(ctx context.Context, cfg ExportConfig) (map[string]int64, error)
}

// exportTables lists the raw ClickHouse tables the hourly export snapshots to
// the lake, each with its event-time column. These mirror the tables the
// (retired, ADR 0006 phase 5) pipeline dual-write used to populate — the
// keep-forever ML corpus + cold archive, now DERIVED from ClickHouse rather than
// written by a second live NATS consumer.
var exportTables = []struct {
	name    string
	timeCol string
}{
	{"impressions", "timestamp"},
	{"clicks", "timestamp"},
	{"conversions", "timestamp"},
	{"views", "timestamp"},
	{"auctions", "timestamp"},
	{"auction_wins", "timestamp"},
	{"dsp_calls", "timestamp"},
	{"behaviour_signals", "observed_at"},
	{"profile_signals", "observed_at"},
	// media_events (video/audio quartile beacons) feed media_starts /
	// media_completes / completion_rate — business metrics, not debug aids, so
	// quartile history must outlive the 30d hot TTL like every other event
	// spine. Exporting also flips its reads to hot+cold routing (hotOnlyTables
	// is derived from this list).
	{"media_events", "timestamp"},
}

// ExportPrefixDefault is the object-key prefix under the lake bucket that the
// ClickHouse→Parquet export writes to (and the s3() cold reader reads back).
// Kept distinct from the Delta-table paths so the two never collide while both
// exist (phase 4 lands before the dual-write is retired in phase 5).
const ExportPrefixDefault = "clickhouse-export"

// ExportConfig locates the S3/Minio target ClickHouse writes Parquet to — the
// same bucket/creds the cold reader reads back through s3().
type ExportConfig struct {
	Endpoint  string // host:port, no scheme (e.g. "minio:9000")
	Bucket    string
	AccessKey string
	SecretKey string
	UseSSL    bool
	Prefix    string // object-key prefix; defaults to ExportPrefixDefault
}

// ExportHourToParquet writes one Parquet object per table for the given hour to
//
//	s3://<bucket>/<prefix>/<table>/dt=YYYY-MM-DD/hour=HH/data.parquet
//
// idempotently: s3_truncate_on_insert overwrites the hour's object on re-run, so
// replaying an hour is safe (the ADR's per-hour idempotence). Returns rows
// exported per table. A zero hour is rejected; hour is truncated to the hour UTC.
func (c *ClickHouse) ExportHourToParquet(ctx context.Context, cfg ExportConfig, hour time.Time) (map[string]int64, error) {
	if hour.IsZero() {
		return nil, fmt.Errorf("export: hour is zero")
	}
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, fmt.Errorf("export: endpoint and bucket are required")
	}
	hour = hour.UTC().Truncate(time.Hour)
	prefix := cfg.Prefix
	if prefix == "" {
		prefix = ExportPrefixDefault
	}
	scheme := "http"
	if cfg.UseSSL {
		scheme = "https"
	}
	dt := hour.Format("2006-01-02")
	hh := hour.Format("15")
	hourStr := hour.Format("2006-01-02 15:04:05")

	out := make(map[string]int64, len(exportTables))
	for _, t := range exportTables {
		url := fmt.Sprintf("%s://%s/%s/%s/%s/dt=%s/hour=%s/data.parquet",
			scheme, cfg.Endpoint, cfg.Bucket, prefix, t.name, dt, hh)
		// INSERT ... FUNCTION s3() with truncate-on-insert = idempotent per hour.
		// Creds are ClickHouse's inline s3() args (config-controlled, not user
		// input); the WHERE bounds the scan to the one hour's partitions.
		insert := fmt.Sprintf(
			"INSERT INTO FUNCTION s3('%s','%s','%s','Parquet') SETTINGS s3_truncate_on_insert=1 "+
				"SELECT * FROM %s WHERE toStartOfHour(%s) = toDateTime('%s')",
			url, cfg.AccessKey, cfg.SecretKey, t.name, t.timeCol, hourStr)
		if _, err := c.db.ExecContext(ctx, insert); err != nil {
			return out, fmt.Errorf("export %s hour %s: %w", t.name, hourStr, err)
		}
		var n int64
		countQ := fmt.Sprintf("SELECT count() FROM %s WHERE toStartOfHour(%s) = toDateTime('%s')",
			t.name, t.timeCol, hourStr)
		if err := c.db.QueryRowContext(ctx, countQ).Scan(&n); err != nil {
			return out, fmt.Errorf("export count %s: %w", t.name, err)
		}
		out[t.name] = n
	}
	return out, nil
}

// ExportSnapshot returns the total exported row count per table across all hour
// partitions in the Parquet export — the "did every event reach the archive?"
// reconciliation that replaces the retired Delta snapshot (ADR 0006 phase 5). A
// table with no export objects yet counts 0 (empty glob → CANNOT_EXTRACT…).
func (c *ClickHouse) ExportSnapshot(ctx context.Context, cfg ExportConfig) (map[string]int64, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, fmt.Errorf("export snapshot: endpoint and bucket are required")
	}
	prefix := cfg.Prefix
	if prefix == "" {
		prefix = ExportPrefixDefault
	}
	scheme := "http"
	if cfg.UseSSL {
		scheme = "https"
	}
	out := make(map[string]int64, len(exportTables))
	for _, t := range exportTables {
		glob := fmt.Sprintf("%s://%s/%s/%s/%s/**/*.parquet", scheme, cfg.Endpoint, cfg.Bucket, prefix, t.name)
		q := fmt.Sprintf("SELECT count() FROM s3('%s','%s','%s','Parquet')", glob, cfg.AccessKey, cfg.SecretKey)
		var n uint64
		if err := c.db.QueryRowContext(ctx, q).Scan(&n); err != nil {
			if isNoParquetFiles(err) {
				out[t.name] = 0
				continue
			}
			return out, fmt.Errorf("export snapshot %s: %w", t.name, err)
		}
		out[t.name] = int64(n)
	}
	return out, nil
}
