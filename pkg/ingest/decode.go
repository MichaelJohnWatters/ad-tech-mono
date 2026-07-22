package ingest

// decode.go — multi-format decode for staged audience files (ADR 0007).
//
// Producers deliver audience files as CSV, TSV, or Parquet, optionally zip- or
// gzip-compressed. Detection is CONTENT-FIRST (magic bytes for zip/gzip/
// parquet), falling back to the file extension and finally a delimiter sniff of
// the header line — provider files are routinely misnamed, so the extension is
// a hint, not a contract. Moved out of cmd/pipeline so the shared processor
// (imported by both the pipeline worker and the gateway) can decode staged
// files itself.

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/pipeline"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/datalake"
)

// maxDecompressedBytes bounds a decompressed staged file (zip-bomb guard).
const maxDecompressedBytes = 256 << 20

// readAllBounded reads up to the decompression bound and ERRORS past it —
// a plain LimitReader would silently truncate an oversized file, ingesting
// a partial audience list as if it were complete.
func readAllBounded(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxDecompressedBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxDecompressedBytes {
		return nil, fmt.Errorf("decompressed size exceeds %d bytes", maxDecompressedBytes)
	}
	return data, nil
}

// DecodeFile turns a raw staged file into pipeline records, auto-detecting
// compression and format. A zip archive's supported entries are concatenated —
// a provider shipping one segment as N part-files inside one archive lands as
// one segment.
func DecodeFile(ctx context.Context, name string, body []byte) ([]pipeline.Record, error) {
	switch {
	case bytes.HasPrefix(body, []byte{0x50, 0x4b, 0x03, 0x04}): // zip
		return decodeZip(ctx, body)
	case bytes.HasPrefix(body, []byte{0x1f, 0x8b}): // gzip
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("gzip: %w", err)
		}
		inner, err := readAllBounded(zr)
		if err != nil {
			return nil, fmt.Errorf("gunzip: %w", err)
		}
		return DecodeFile(ctx, strings.TrimSuffix(name, ".gz"), inner)
	default:
		return decodeFlat(ctx, name, body)
	}
}

func decodeZip(ctx context.Context, body []byte) ([]pipeline.Record, error) {
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil, fmt.Errorf("zip: %w", err)
	}
	var out []pipeline.Record
	decoded := 0
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || strings.HasPrefix(f.FileInfo().Name(), ".") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("zip entry %s: %w", f.Name, err)
		}
		inner, err := readAllBounded(rc)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("zip entry %s: %w", f.Name, err)
		}
		recs, err := decodeFlat(ctx, f.Name, inner)
		if err != nil {
			return nil, fmt.Errorf("zip entry %s: %w", f.Name, err)
		}
		out = append(out, recs...)
		decoded++
	}
	if decoded == 0 {
		return nil, fmt.Errorf("zip contains no supported files")
	}
	return out, nil
}

// decodeFlat handles an uncompressed payload: parquet by magic, else
// delimiter-separated text.
func decodeFlat(ctx context.Context, name string, body []byte) ([]pipeline.Record, error) {
	if bytes.HasPrefix(body, []byte("PAR1")) {
		return parquetToPipelineRecords(ctx, body)
	}
	switch strings.ToLower(pathExt(name)) {
	case ".parquet":
		return nil, fmt.Errorf("%s: named .parquet but missing PAR1 magic", name)
	case ".tsv":
		return pipeline.IngestCSV(bytes.NewReader(body), '\t')
	case ".csv":
		return pipeline.IngestCSV(bytes.NewReader(body), ',')
	}
	// Unknown extension — sniff the header line's delimiter.
	header := body
	if i := bytes.IndexByte(body, '\n'); i > 0 {
		header = body[:i]
	}
	if bytes.Count(header, []byte{'\t'}) > bytes.Count(header, []byte{','}) {
		return pipeline.IngestCSV(bytes.NewReader(body), '\t')
	}
	return pipeline.IngestCSV(bytes.NewReader(body), ',')
}

// parquetToPipelineRecords flattens typed Parquet rows into the string-map
// records the validation pipeline works on.
func parquetToPipelineRecords(ctx context.Context, body []byte) ([]pipeline.Record, error) {
	rows, err := datalake.ParquetRecords(ctx, body)
	if err != nil {
		return nil, fmt.Errorf("parquet: %w", err)
	}
	out := make([]pipeline.Record, 0, len(rows))
	for _, row := range rows {
		rec := make(pipeline.Record, len(row))
		for k, v := range row {
			rec[strings.ToLower(k)] = stringifyValue(v)
		}
		out = append(out, rec)
	}
	return out, nil
}

func stringifyValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case time.Time:
		return t.UTC().Format(time.RFC3339)
	default:
		return fmt.Sprint(t)
	}
}

// SegmentNameFromFile strips compression + format extensions so
// "auto-intenders.csv.gz" names the segment "auto-intenders".
func SegmentNameFromFile(base string) string {
	for _, ext := range []string{".gz", ".zip", ".csv", ".tsv", ".parquet"} {
		base = strings.TrimSuffix(base, ext)
	}
	return base
}

func pathExt(name string) string {
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		return name[i:]
	}
	return ""
}
