package ingest

import (
	"encoding/csv"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/pipeline"
)

func TestRenderRejectedCSV(t *testing.T) {
	rows := []pipeline.QuarantineRecord{
		{
			Record: pipeline.Record{"id_value": "", "geo": "US"},
			Errors: []pipeline.ValidationError{{Field: "id_value", Rule: "required", Message: "field is required but missing or empty"}},
		},
	}
	body, err := renderRejectedCSV(rows)
	if err != nil {
		t.Fatalf("renderRejectedCSV: %v", err)
	}
	recs, err := csv.NewReader(strings.NewReader(string(body))).ReadAll()
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("want header + 1 row, got %d", len(recs))
	}
	header := strings.Join(recs[0], ",")
	if header != "geo,id_value,_errors" {
		t.Fatalf("unexpected header order: %s", header)
	}
	if !strings.Contains(recs[1][2], "required") {
		t.Fatalf("errors column missing rule: %q", recs[1][2])
	}
}
