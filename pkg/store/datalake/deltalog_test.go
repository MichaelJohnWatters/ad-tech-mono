package datalake

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDeltaSchemaString_TypeMapping(t *testing.T) {
	s := Schema{Columns: []Column{
		{Name: "publisher_id", Type: "string", Nullable: true},
		{Name: "num_bids", Type: "int64", Nullable: true},
		{Name: "clearing_price_usd", Type: "float64", Nullable: false},
		{Name: "viewable", Type: "bool", Nullable: true},
		{Name: "timestamp", Type: "timestamp", Nullable: true},
	}}
	got, err := deltaSchemaString(s)
	if err != nil {
		t.Fatal(err)
	}
	// schemaString is itself JSON — decode and check the Spark type names.
	var parsed struct {
		Type   string `json:"type"`
		Fields []struct {
			Name     string `json:"name"`
			Type     string `json:"type"`
			Nullable bool   `json:"nullable"`
		} `json:"fields"`
	}
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("schemaString is not valid JSON: %v\n%s", err, got)
	}
	if parsed.Type != "struct" {
		t.Errorf("root type = %q, want struct", parsed.Type)
	}
	want := map[string]string{
		"publisher_id": "string", "num_bids": "long", "clearing_price_usd": "double",
		"viewable": "boolean", "timestamp": "timestamp",
	}
	if len(parsed.Fields) != len(want) {
		t.Fatalf("got %d fields, want %d", len(parsed.Fields), len(want))
	}
	for _, f := range parsed.Fields {
		if want[f.Name] != f.Type {
			t.Errorf("field %s type = %q, want %q", f.Name, f.Type, want[f.Name])
		}
	}
}

func TestBuildDeltaCommit0_Shape(t *testing.T) {
	schema := Schema{Columns: []Column{{Name: "publisher_id", Type: "string", Nullable: true}}}
	add := deltaAdd{Path: "part-00000.parquet", PartitionValues: map[string]string{}, Size: 1234, ModificationTime: 1000, DataChange: true}
	b, err := buildDeltaCommit0("table-uuid", schema, add, 900)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("commit0 should have 3 action lines (protocol/metaData/add), got %d:\n%s", len(lines), b)
	}
	// Line 1 = protocol, line 2 = metaData, line 3 = add — each a single-key wrapper.
	for i, wantKey := range []string{"protocol", "metaData", "add"} {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(lines[i]), &m); err != nil {
			t.Fatalf("line %d not JSON: %v", i, err)
		}
		if _, ok := m[wantKey]; !ok || len(m) != 1 {
			t.Errorf("line %d = %s, want single key %q", i, lines[i], wantKey)
		}
	}
}

func TestBuildDeltaCommitCompact_AddThenRemoves(t *testing.T) {
	add := deltaAdd{Path: "part-00003.parquet", PartitionValues: map[string]string{}, Size: 10, DataChange: true}
	removed := []deltaRemove{
		{Path: "part-00000.parquet", DeletionTimestamp: 1, DataChange: true},
		{Path: "part-00001.parquet", DeletionTimestamp: 1, DataChange: true},
	}
	b, err := buildDeltaCommitCompact(add, removed)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("compact commit = 1 add + 2 removes = 3 lines, got %d", len(lines))
	}
	if !strings.HasPrefix(lines[0], `{"add"`) {
		t.Errorf("first action should be the add, got %s", lines[0])
	}
	if !strings.Contains(lines[1], `"remove"`) || !strings.Contains(lines[2], `"remove"`) {
		t.Errorf("lines 2-3 should be removes, got %s / %s", lines[1], lines[2])
	}
}
