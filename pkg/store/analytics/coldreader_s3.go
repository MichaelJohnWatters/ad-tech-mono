package analytics

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// CHParquetColdReader answers deep-history reporting queries from the Parquet
// export (ADR 0006 phases 3-4) via ClickHouse's s3() table function. It reuses
// the SAME aggregation builder (BuildQueryFrom) and SAME engine as the hot
// path — just swapping the FROM target from a MergeTree table to an s3() glob
// over the export objects — so hot/cold parity is structural, not a
// re-implementation. Pure Go (clickhouse-go/v2), so reporting no longer needs
// the CGO `duckdb` build tag. Implements ColdReader.
type CHParquetColdReader struct {
	db     *sql.DB
	s3     ExportConfig
	prefix string
}

// NewCHParquetColdReader opens a ClickHouse connection used purely to run s3()
// reads over the Parquet export. chCfg points at the same ClickHouse the hot
// store uses; s3 locates the export objects (endpoint/bucket/creds).
func NewCHParquetColdReader(chCfg ClickHouseConfig, s3 ExportConfig) (*CHParquetColdReader, error) {
	db := clickhouse.OpenDB(&clickhouse.Options{
		Addr: chCfg.Addrs,
		Auth: clickhouse.Auth{
			Database: chCfg.Database,
			Username: chCfg.Username,
			Password: chCfg.Password,
		},
	})
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("clickhouse cold reader ping: %w", err)
	}
	prefix := s3.Prefix
	if prefix == "" {
		prefix = ExportPrefixDefault
	}
	return &CHParquetColdReader{db: db, s3: s3, prefix: prefix}, nil
}

func (r *CHParquetColdReader) Close() error { return r.db.Close() }

// Query runs the standard aggregation over the s3() Parquet export for the
// params' table. A glob matching NO objects (a table/hour never exported) is a
// legitimately-empty cold window, so it returns an empty result rather than an
// error — otherwise the HotColdStore would drop the whole cold half for a
// merely-sparse table.
func (r *CHParquetColdReader) Query(ctx context.Context, params QueryParams) (*QueryResult, error) {
	if strings.TrimSpace(params.Table) == "" {
		return &QueryResult{}, nil
	}
	scheme := "http"
	if r.s3.UseSSL {
		scheme = "https"
	}
	// One glob across every hour partition for this table; s3() reads them as a
	// single table. Path mirrors ExportHourToParquet's layout.
	glob := fmt.Sprintf("%s://%s/%s/%s/%s/**/*.parquet",
		scheme, r.s3.Endpoint, r.s3.Bucket, r.prefix, params.Table)
	fromExpr := fmt.Sprintf("s3('%s','%s','%s','Parquet')", glob, r.s3.AccessKey, r.s3.SecretKey)

	query, args := BuildQueryFrom(params, fromExpr)
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		if isNoParquetFiles(err) {
			return &QueryResult{Columns: nil}, nil
		}
		return nil, fmt.Errorf("cold s3 query: %w", err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("cold columns: %w", err)
	}
	result := &QueryResult{Columns: cols}
	for rows.Next() {
		vals := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("cold scan: %w", err)
		}
		result.Rows = append(result.Rows, vals)
	}
	return result, rows.Err()
}

// isNoParquetFiles reports whether err is ClickHouse's "no files at path" for an
// s3() glob that matched nothing (CANNOT_EXTRACT_TABLE_STRUCTURE, code 636) — it
// can't infer a schema with zero files, which for a cold read just means the
// window is empty.
func isNoParquetFiles(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "no files with provided path") ||
		strings.Contains(msg, "CANNOT_EXTRACT_TABLE_STRUCTURE")
}
