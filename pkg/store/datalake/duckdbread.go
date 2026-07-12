//go:build duckdb

// DuckDB-over-Parquet read surface — the serverless ad-hoc / historical / ML
// query path over the Parquet cold archive on S3/Minio (ADR 0001/0002). DuckDB
// reads the same open files the pipeline writes: no data movement, no server.
//
// NB: our lake uses a home-grown _delta_log (one custom transaction JSON per
// file — see objstore.go), NOT the real Delta Lake protocol. So DuckDB's
// delta_scan() cannot read it (it needs protocol/metaData actions we don't
// emit). The cold tier (ColdStore) instead resolves the ACTIVE file set from
// that log via ObjectStore.Snapshot and reads exactly those parts with
// read_parquet([...]) — correct AND tombstone-aware after compaction. (Real
// delta_scan/Spark interop would need the writer to emit proper Delta logs — a
// future migration, not needed by the cold tier.)
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
	// The delta extension's Rust kernel does NOT read the legacy SET s3_*
	// settings above (those only reach httpfs/read_parquet). Without an explicit
	// entry in the DuckDB Secrets manager it falls back to the EC2 instance
	// metadata provider (169.254.169.254) and fails against Minio/any non-AWS S3.
	// Register an S3 secret so delta_scan authenticates the same way httpfs does.
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	setup = append(setup, fmt.Sprintf(
		"CREATE OR REPLACE SECRET adtech_s3 (TYPE S3, KEY_ID '%s', SECRET '%s', ENDPOINT '%s', REGION '%s', URL_STYLE 'path', USE_SSL %t)",
		cfg.AccessKey, cfg.SecretKey, cfg.Endpoint, region, cfg.UseSSL,
	))
	for _, s := range setup {
		if _, err := db.ExecContext(ctx, s); err != nil {
			db.Close()
			return nil, fmt.Errorf("duckdb setup %q: %w", s, err)
		}
	}
	return &ParquetReader{db: db, bucket: bucket}, nil
}

func (r *ParquetReader) Close() error { return r.db.Close() }

// tableExists reports whether the table has a Delta log (at least one commit
// file). A table the pipeline hasn't written yet has no _delta_log, and
// delta_scan on it errors — ColdStore uses this to return an empty result
// instead of a hard failure.
func (r *ParquetReader) tableExists(ctx context.Context, table string) bool {
	var n int64
	err := r.db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT count(*) FROM glob('s3://%s/%s/_delta_log/*.json')", r.bucket, table)).Scan(&n)
	return err == nil && n > 0
}

// Query runs arbitrary SQL and returns rows as maps. The live caller is
// ColdStore, which builds a delta_scan('s3://…/<table>') FROM clause; delta_scan
// resolves the active file set from the Delta log, e.g.
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
