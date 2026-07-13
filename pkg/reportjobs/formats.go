package reportjobs

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

// FormatWriter renders a query result as one downloadable artifact.
type FormatWriter interface {
	ContentType() string
	Ext() string
	Write(w io.Writer, res analytics.QueryResult) error
}

// WriterFor returns the writer for an artifact format.
func WriterFor(format string) (FormatWriter, error) {
	switch format {
	case FormatCSV:
		return csvWriter{}, nil
	case FormatJSON:
		return jsonWriter{}, nil
	case FormatParquet:
		return parquetWriter{}, nil
	}
	return nil, fmt.Errorf("unsupported report format %q", format)
}

// --- CSV ---

type csvWriter struct{}

func (csvWriter) ContentType() string { return "text/csv" }
func (csvWriter) Ext() string         { return "csv" }

func (csvWriter) Write(w io.Writer, res analytics.QueryResult) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(res.Columns); err != nil {
		return err
	}
	cells := make([]string, len(res.Columns))
	for _, row := range res.Rows {
		for i := range cells {
			cells[i] = ""
			if i < len(row) && row[i] != nil {
				cells[i] = fmt.Sprintf("%v", row[i])
			}
		}
		if err := cw.Write(cells); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// --- JSON ---

type jsonWriter struct{}

func (jsonWriter) ContentType() string { return "application/json" }
func (jsonWriter) Ext() string         { return "json" }

// Write emits an array of {column: value} objects — the shape API consumers
// expect, rather than the internal columns/rows split.
func (jsonWriter) Write(w io.Writer, res analytics.QueryResult) error {
	enc := json.NewEncoder(w)
	out := make([]map[string]any, 0, len(res.Rows))
	for _, row := range res.Rows {
		obj := make(map[string]any, len(res.Columns))
		for i, col := range res.Columns {
			if i < len(row) {
				obj[col] = row[i]
			} else {
				obj[col] = nil
			}
		}
		out = append(out, obj)
	}
	return enc.Encode(out)
}

// --- Parquet ---

type parquetWriter struct{}

func (parquetWriter) ContentType() string { return "application/vnd.apache.parquet" }
func (parquetWriter) Ext() string         { return "parquet" }

// Write builds a single arrow record and writes it as one parquet file. Column
// types are inferred from the first non-nil value per column: rows have been
// JSON-round-tripped through the reporting API, so every numeric arrives as
// float64 (int-like columns become doubles) and everything else is stringly.
// All-null columns default to string.
func (parquetWriter) Write(w io.Writer, res analytics.QueryResult) error {
	mem := memory.NewGoAllocator()
	fields := make([]arrow.Field, len(res.Columns))
	for i, col := range res.Columns {
		fields[i] = arrow.Field{Name: col, Type: inferArrowType(res.Rows, i), Nullable: true}
	}
	schema := arrow.NewSchema(fields, nil)

	b := array.NewRecordBuilder(mem, schema)
	defer b.Release()
	for _, row := range res.Rows {
		for i := range fields {
			var v any
			if i < len(row) {
				v = row[i]
			}
			appendCell(b.Field(i), v)
		}
	}
	rec := b.NewRecord()
	defer rec.Release()
	tbl := array.NewTableFromRecords(schema, []arrow.Record{rec})
	defer tbl.Release()

	chunk := int64(len(res.Rows))
	if chunk == 0 {
		chunk = 1
	}
	return pqarrow.WriteTable(tbl, w, chunk,
		parquet.NewWriterProperties(parquet.WithAllocator(mem)),
		pqarrow.DefaultWriterProps())
}

func inferArrowType(rows [][]any, col int) arrow.DataType {
	for _, row := range rows {
		if col >= len(row) || row[col] == nil {
			continue
		}
		switch row[col].(type) {
		case float64, float32, int, int32, int64:
			return arrow.PrimitiveTypes.Float64
		case bool:
			return arrow.FixedWidthTypes.Boolean
		default:
			return arrow.BinaryTypes.String
		}
	}
	return arrow.BinaryTypes.String
}

func appendCell(fb array.Builder, v any) {
	if v == nil {
		fb.AppendNull()
		return
	}
	switch b := fb.(type) {
	case *array.Float64Builder:
		switch n := v.(type) {
		case float64:
			b.Append(n)
		case float32:
			b.Append(float64(n))
		case int:
			b.Append(float64(n))
		case int32:
			b.Append(float64(n))
		case int64:
			b.Append(float64(n))
		default:
			b.AppendNull()
		}
	case *array.BooleanBuilder:
		if n, ok := v.(bool); ok {
			b.Append(n)
		} else {
			b.AppendNull()
		}
	case *array.StringBuilder:
		b.Append(fmt.Sprintf("%v", v))
	default:
		fb.AppendNull()
	}
}

// renderArtifact renders res in the given format and returns the bytes plus
// the writer (for content type / extension).
func renderArtifact(format string, res analytics.QueryResult) ([]byte, FormatWriter, error) {
	fw, err := WriterFor(format)
	if err != nil {
		return nil, nil, err
	}
	var buf bytes.Buffer
	if err := fw.Write(&buf, res); err != nil {
		return nil, nil, fmt.Errorf("render %s: %w", format, err)
	}
	return buf.Bytes(), fw, nil
}
