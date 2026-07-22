package analytics

import (
	"errors"
	"testing"
)

func TestIsNoParquetFiles(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated", errors.New("connection refused"), false},
		{
			"clickhouse zero-files phrase",
			errors.New("Code: 636. DB::Exception: The table structure cannot be extracted from a Parquet format file, because there are no files with provided path in S3ObjectStorage or all files are empty."),
			true,
		},
		{
			"clickhouse error code name",
			errors.New("CANNOT_EXTRACT_TABLE_STRUCTURE"),
			true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isNoParquetFiles(c.err); got != c.want {
				t.Fatalf("isNoParquetFiles(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
