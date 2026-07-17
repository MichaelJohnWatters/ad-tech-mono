package datalake

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
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

func TestDeltaSchemaString_PartitionedAddsEventDate(t *testing.T) {
	s := Schema{
		Columns:     []Column{{Name: "observed_at", Type: "timestamp", Nullable: true}},
		PartitionBy: "observed_at",
	}
	got, err := deltaSchemaString(s)
	if err != nil {
		t.Fatal(err)
	}
	// Delta requires partition columns in the table schema even though their
	// values live in file paths, not in the Parquet.
	if !strings.Contains(got, `"name":"event_date"`) || !strings.Contains(got, `"type":"date"`) {
		t.Errorf("partitioned schemaString should carry event_date:date, got %s", got)
	}
}

func TestBuildDeltaCommit_Version0Shape(t *testing.T) {
	schema := Schema{Columns: []Column{{Name: "publisher_id", Type: "string", Nullable: true}}}
	md, err := deltaMetaDataFor("table-uuid", schema, 900)
	if err != nil {
		t.Fatal(err)
	}
	add := deltaAdd{Path: "part-00000-000.parquet", PartitionValues: map[string]string{}, Size: 1234, ModificationTime: 1000, DataChange: true}
	b, err := buildDeltaCommit(true, &md, []deltaAdd{add}, nil)
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

func TestBuildDeltaCommit_AddsThenRemoves(t *testing.T) {
	adds := []deltaAdd{{Path: "event_date=2026-07-17/part-00003-000.parquet", PartitionValues: map[string]string{"event_date": "2026-07-17"}, Size: 10, DataChange: true}}
	removed := []deltaRemove{
		{Path: "part-00000-000.parquet", DeletionTimestamp: 1, DataChange: true},
		{Path: "part-00001-000.parquet", DeletionTimestamp: 1, DataChange: true},
	}
	b, err := buildDeltaCommit(false, nil, adds, removed)
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

func TestPartitionMetadata_RoundTrip(t *testing.T) {
	schema := Schema{
		Columns: []Column{
			{Name: "user_id", Type: "string", Nullable: true},
			{Name: "observed_at", Type: "timestamp", Nullable: true},
		},
		PartitionBy: "observed_at",
	}
	md, err := deltaMetaDataFor("table-uuid", schema, 900)
	if err != nil {
		t.Fatal(err)
	}
	if len(md.PartitionColumns) != 1 || md.PartitionColumns[0] != "event_date" {
		t.Fatalf("partitionColumns = %v, want [event_date]", md.PartitionColumns)
	}
	add := deltaAdd{Path: "event_date=2026-07-17/part-00000-000.parquet", PartitionValues: map[string]string{"event_date": "2026-07-17"}, DataChange: true}
	b, err := buildDeltaCommit(true, &md, []deltaAdd{add}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := parseDeltaCommit(b)
	if err != nil {
		t.Fatal(err)
	}
	// The recovered Go schema must exclude event_date (never in the Parquet)
	// and carry the partition source back out of the configuration bag.
	if c.schema == nil {
		t.Fatal("no schema recovered")
	}
	if c.schema.PartitionBy != "observed_at" {
		t.Errorf("recovered PartitionBy = %q, want observed_at", c.schema.PartitionBy)
	}
	for _, col := range c.schema.Columns {
		if col.Name == "event_date" {
			t.Error("event_date leaked into the recovered Go schema")
		}
	}
	if len(c.schema.Columns) != 2 {
		t.Errorf("recovered %d columns, want 2", len(c.schema.Columns))
	}
}

func TestDeltaStats_MinMax(t *testing.T) {
	t1 := time.Date(2026, 7, 17, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 7, 17, 12, 30, 0, 0, time.UTC)
	s := deltaStats([]Record{{"observed_at": t2}, {"observed_at": t1}}, "observed_at")
	if !strings.Contains(s, `"numRecords":2`) {
		t.Errorf("stats missing numRecords: %s", s)
	}
	if !strings.Contains(s, `"minValues":{"observed_at":"2026-07-17T10:00:00.000Z"}`) {
		t.Errorf("stats missing minValues: %s", s)
	}
	if !strings.Contains(s, `"maxValues":{"observed_at":"2026-07-17T12:30:00.000Z"}`) {
		t.Errorf("stats missing maxValues: %s", s)
	}
	// Unpartitioned / no usable values → row count only.
	if got := deltaStats([]Record{{"x": 1}}, ""); got != `{"numRecords":1}` {
		t.Errorf("plain stats = %s", got)
	}
	if got := deltaStats([]Record{{"x": 1}}, "observed_at"); got != `{"numRecords":1}` {
		t.Errorf("no-value stats = %s", got)
	}
}
