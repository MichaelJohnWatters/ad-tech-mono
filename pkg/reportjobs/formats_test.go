package reportjobs

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

// fixtureResult mixes float64 (JSON-round-tripped numerics), strings, bools
// and nils — the shapes the reporting API actually returns.
func fixtureResult() analytics.QueryResult {
	return analytics.QueryResult{
		Columns: []string{"campaign_id", "count", "viewable"},
		Rows: [][]any{
			{"camp-1", float64(42), true},
			{"camp-2", float64(7), false},
			{"camp-3", nil, nil},
		},
	}
}

func TestCSVWriter(t *testing.T) {
	data, fw, err := renderArtifact(FormatCSV, fixtureResult())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if fw.ContentType() != "text/csv" || fw.Ext() != "csv" {
		t.Errorf("meta: %s/%s", fw.ContentType(), fw.Ext())
	}
	want := "campaign_id,count,viewable\ncamp-1,42,true\ncamp-2,7,false\ncamp-3,,\n"
	if string(data) != want {
		t.Errorf("csv output:\n%s\nwant:\n%s", data, want)
	}
}

func TestJSONWriter(t *testing.T) {
	data, fw, err := renderArtifact(FormatJSON, fixtureResult())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if fw.ContentType() != "application/json" {
		t.Errorf("content type %s", fw.ContentType())
	}
	var rows []map[string]any
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	if rows[0]["campaign_id"] != "camp-1" || rows[0]["count"] != float64(42) || rows[0]["viewable"] != true {
		t.Errorf("row 0: %+v", rows[0])
	}
	if rows[2]["count"] != nil {
		t.Errorf("row 2 count = %v, want nil", rows[2]["count"])
	}
}

func TestJSONWriterEmptyResult(t *testing.T) {
	data, _, err := renderArtifact(FormatJSON, analytics.QueryResult{Columns: []string{"a"}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.TrimSpace(string(data)) != "[]" {
		t.Errorf("empty result rendered %q, want []", data)
	}
}

func TestParquetWriterRoundTrip(t *testing.T) {
	res := fixtureResult()
	data, fw, err := renderArtifact(FormatParquet, res)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if fw.Ext() != "parquet" {
		t.Errorf("ext %s", fw.Ext())
	}

	mem := memory.NewGoAllocator()
	tbl, err := pqarrow.ReadTable(context.Background(), bytes.NewReader(data),
		parquet.NewReaderProperties(mem), pqarrow.ArrowReadProperties{}, mem)
	if err != nil {
		t.Fatalf("read back parquet: %v", err)
	}
	defer tbl.Release()

	if got := int(tbl.NumRows()); got != len(res.Rows) {
		t.Errorf("rows: %d, want %d", got, len(res.Rows))
	}
	if got := int(tbl.NumCols()); got != len(res.Columns) {
		t.Errorf("cols: %d, want %d", got, len(res.Columns))
	}
	for i, want := range res.Columns {
		if got := tbl.Column(i).Name(); got != want {
			t.Errorf("col %d name %s, want %s", i, got, want)
		}
	}
	// Type inference: campaign_id string, count float64, viewable bool.
	types := []string{"utf8", "float64", "bool"}
	for i, want := range types {
		if got := tbl.Column(i).DataType().Name(); got != want {
			t.Errorf("col %d type %s, want %s", i, got, want)
		}
	}
}

func TestParquetWriterEmptyResult(t *testing.T) {
	if _, _, err := renderArtifact(FormatParquet, analytics.QueryResult{Columns: []string{"a", "b"}}); err != nil {
		t.Fatalf("empty parquet render: %v", err)
	}
}

func TestWriterForUnknownFormat(t *testing.T) {
	if _, err := WriterFor("xlsx"); err == nil {
		t.Error("expected error for unsupported format")
	}
	for _, f := range []string{FormatCSV, FormatJSON, FormatParquet} {
		if _, err := WriterFor(f); err != nil {
			t.Errorf("WriterFor(%s): %v", f, err)
		}
	}
}
