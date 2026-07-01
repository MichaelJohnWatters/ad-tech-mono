//go:build duckdb

// DuckDB-over-Parquet read surface — the serverless ad-hoc / historical / ML
// query path over the Parquet+Delta cold archive on S3/Minio (ADR 0001/0002).
// DuckDB reads the same open files the pipeline writes: no data movement, no
// server. delta_scan honours the _delta_log (skips tombstoned parts after
// compaction); read_parquet globs raw files for a quick pre-compaction look.
//
// Requires the `duckdb` build tag (CGO driver) plus network access the first
// time (httpfs/delta extensions are downloaded by INSTALL).

package datalake

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/marcboeker/go-duckdb"
)

// S3Config points the reader at an S3/Minio endpoint.
type S3Config struct {
	Endpoint  string // host:port, no scheme (Minio local: "localhost:9000")
	AccessKey string
	SecretKey string
	UseSSL    bool
	Region    string // optional
}

// ParquetReader is a DuckDB handle configured to read the lake over S3.
type ParquetReader struct {
	db     *sql.DB
	bucket string
}

// NewParquetReader opens an in-memory DuckDB, loads httpfs + delta, and points
// them at the given S3/Minio endpoint. Close it when done.
func NewParquetReader(cfg S3Config, bucket string) (*ParquetReader, error) {
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, fmt.Errorf("open duckdb: %w", err)
	}
	ctx := context.Background()
	setup := []string{
		"INSTALL httpfs", "LOAD httpfs",
		"INSTALL delta", "LOAD delta",
		fmt.Sprintf("SET s3_endpoint='%s'", cfg.Endpoint),
		fmt.Sprintf("SET s3_access_key_id='%s'", cfg.AccessKey),
		fmt.Sprintf("SET s3_secret_access_key='%s'", cfg.SecretKey),
		"SET s3_url_style='path'", // Minio needs path-style addressing
		fmt.Sprintf("SET s3_use_ssl=%t", cfg.UseSSL),
	}
	if cfg.Region != "" {
		setup = append(setup, fmt.Sprintf("SET s3_region='%s'", cfg.Region))
	}
	for _, s := range setup {
		if _, err := db.ExecContext(ctx, s); err != nil {
			db.Close()
			return nil, fmt.Errorf("duckdb setup %q: %w", s, err)
		}
	}
	return &ParquetReader{db: db, bucket: bucket}, nil
}

func (r *ParquetReader) Close() error { return r.db.Close() }

// DeltaScanURI returns the delta_scan target for a table (its root, which holds
// _delta_log). The correct reader once compaction rewrites files — it resolves
// the transaction log and skips tombstoned parts (no double-count).
func (r *ParquetReader) DeltaScanURI(table string) string {
	return fmt.Sprintf("s3://%s/%s", r.bucket, table)
}

// ParquetGlobURI returns the read_parquet glob for a table (all part files,
// blind to the Delta log). Fine for a pre-compaction quick look; prefer
// DeltaScanURI once removes are in play.
func (r *ParquetReader) ParquetGlobURI(table string) string {
	return fmt.Sprintf("s3://%s/%s/*.parquet", r.bucket, table)
}

// Query runs arbitrary SQL and returns rows as maps. Callers build the FROM
// clause with DeltaScanURI/ParquetGlobURI, e.g.
//
//	SELECT campaign_id, count(*) FROM delta_scan('s3://…/impressions') GROUP BY 1
func (r *ParquetReader) Query(ctx context.Context, query string, args ...any) ([]map[string]any, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		m := make(map[string]any, len(cols))
		for i, c := range cols {
			m[c] = vals[i]
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CountDeltaScan returns the total row count of a table via delta_scan — the
// on-disk figure that should reconcile with the reporting/NATS event count for
// the archived window (the zero-slippage check across the hot/cold boundary).
func (r *ParquetReader) CountDeltaScan(ctx context.Context, table string) (int64, error) {
	var n int64
	err := r.db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT count(*) FROM delta_scan('%s')", r.DeltaScanURI(table))).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count delta_scan %s: %w", table, err)
	}
	return n, nil
}
