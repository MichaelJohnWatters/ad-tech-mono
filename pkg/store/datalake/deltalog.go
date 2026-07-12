package datalake

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Real Delta Lake transaction-log encoding/decoding (protocol / metaData / add /
// remove actions, newline-delimited JSON). This is what ObjectStore.Write and
// Compact commit and what Log reads back, so the lake is a genuine Delta table —
// readable both by our own reader and by STANDARD Delta readers (Spark, Trino,
// DuckDB delta_scan). Verified by deltalog_test.go (shape) and the delta_scan
// integration tests.

// A commit file is a list of these action objects, one JSON object per line.
// Each concrete action is emitted as a single-key wrapper object, e.g.
// {"add": {...}}, which is the Delta on-disk shape.

type deltaProtocol struct {
	MinReaderVersion int `json:"minReaderVersion"`
	MinWriterVersion int `json:"minWriterVersion"`
}

type deltaFormat struct {
	Provider string            `json:"provider"`
	Options  map[string]string `json:"options"`
}

type deltaMetaData struct {
	ID               string            `json:"id"`
	Format           deltaFormat       `json:"format"`
	SchemaString     string            `json:"schemaString"`
	PartitionColumns []string          `json:"partitionColumns"`
	Configuration    map[string]string `json:"configuration"`
	CreatedTime      int64             `json:"createdTime"`
}

type deltaAdd struct {
	Path             string            `json:"path"`
	PartitionValues  map[string]string `json:"partitionValues"`
	Size             int64             `json:"size"`
	ModificationTime int64             `json:"modificationTime"`
	DataChange       bool              `json:"dataChange"`
	// Stats is Delta's per-file statistics as a JSON string, e.g.
	// {"numRecords":5}. We populate numRecords so Snapshot can report row counts
	// without opening the Parquet, and delta_scan can use it for planning.
	Stats string `json:"stats,omitempty"`
}

// deltaStats renders the minimal Delta per-file stats string (row count only).
func deltaStats(numRecords int) string {
	return fmt.Sprintf(`{"numRecords":%d}`, numRecords)
}

// numRecordsFromStats extracts numRecords from a Delta add-stats string (0 if
// absent/unparseable).
func numRecordsFromStats(s string) int {
	if s == "" {
		return 0
	}
	var st struct {
		NumRecords int `json:"numRecords"`
	}
	_ = json.Unmarshal([]byte(s), &st)
	return st.NumRecords
}

type deltaRemove struct {
	Path              string `json:"path"`
	DeletionTimestamp int64  `json:"deletionTimestamp"`
	DataChange        bool   `json:"dataChange"`
}

// deltaType maps our Schema column type to the Delta/Spark type name used in the
// schemaString.
func deltaType(t string) string {
	switch t {
	case "int64":
		return "long"
	case "float64":
		return "double"
	case "bool":
		return "boolean"
	case "timestamp":
		return "timestamp"
	default: // "string" and anything unknown
		return "string"
	}
}

// deltaSchemaString renders our Schema as a Delta schemaString — itself a
// JSON-encoded Spark struct type, embedded as a string inside the metaData
// action.
func deltaSchemaString(schema Schema) (string, error) {
	type field struct {
		Name     string                 `json:"name"`
		Type     string                 `json:"type"`
		Nullable bool                   `json:"nullable"`
		Metadata map[string]interface{} `json:"metadata"`
	}
	type structType struct {
		Type   string  `json:"type"`
		Fields []field `json:"fields"`
	}
	st := structType{Type: "struct", Fields: make([]field, 0, len(schema.Columns))}
	for _, c := range schema.Columns {
		st.Fields = append(st.Fields, field{
			Name: c.Name, Type: deltaType(c.Type), Nullable: c.Nullable, Metadata: map[string]interface{}{},
		})
	}
	b, err := json.Marshal(st)
	return string(b), err
}

// buildDeltaCommit0 builds the version-0 commit: protocol + metaData + the
// table's first add. tableID is a stable UUID; createdMs/modMs are epoch millis.
func buildDeltaCommit0(tableID string, schema Schema, add deltaAdd, createdMs int64) ([]byte, error) {
	schemaStr, err := deltaSchemaString(schema)
	if err != nil {
		return nil, err
	}
	return encodeDeltaActions([]interface{}{
		map[string]deltaProtocol{"protocol": {MinReaderVersion: 1, MinWriterVersion: 2}},
		map[string]deltaMetaData{"metaData": {
			ID:               tableID,
			Format:           deltaFormat{Provider: "parquet", Options: map[string]string{}},
			SchemaString:     schemaStr,
			PartitionColumns: []string{},
			Configuration:    map[string]string{},
			CreatedTime:      createdMs,
		}},
		map[string]deltaAdd{"add": add},
	})
}

// buildDeltaCommitAdd builds a single-add commit (a normal append).
func buildDeltaCommitAdd(add deltaAdd) ([]byte, error) {
	return encodeDeltaActions([]interface{}{map[string]deltaAdd{"add": add}})
}

// buildDeltaCommitCompact builds ONE atomic compaction commit: the consolidated
// add plus a remove for every superseded file. (Our current Compact spreads
// these across N+1 versions — the real Delta single commit is atomic.)
func buildDeltaCommitCompact(add deltaAdd, removed []deltaRemove) ([]byte, error) {
	actions := make([]interface{}, 0, 1+len(removed))
	actions = append(actions, map[string]deltaAdd{"add": add})
	for _, r := range removed {
		actions = append(actions, map[string]deltaRemove{"remove": r})
	}
	return encodeDeltaActions(actions)
}

// encodeDeltaActions serialises actions as newline-delimited JSON (the Delta
// commit-file wire format).
func encodeDeltaActions(actions []interface{}) ([]byte, error) {
	var b strings.Builder
	for _, a := range actions {
		line, err := json.Marshal(a)
		if err != nil {
			return nil, err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return []byte(b.String()), nil
}

// deltaCommit is a parsed commit file: the schema (only when the commit carried
// a metaData action) plus its add/remove actions in file order.
type deltaCommit struct {
	schema  *Schema
	adds    []deltaAdd
	removes []deltaRemove
}

// parseDeltaCommit decodes one newline-delimited Delta commit file into its
// actions. protocol actions are ignored; metaData yields the schema.
func parseDeltaCommit(body []byte) (deltaCommit, error) {
	var c deltaCommit
	for _, line := range strings.Split(strings.TrimRight(string(body), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			return c, fmt.Errorf("delta commit line: %w", err)
		}
		if raw, ok := m["metaData"]; ok {
			var md deltaMetaData
			if err := json.Unmarshal(raw, &md); err != nil {
				return c, fmt.Errorf("delta metaData: %w", err)
			}
			s, err := parseDeltaSchemaString(md.SchemaString)
			if err != nil {
				return c, err
			}
			c.schema = &s
		}
		if raw, ok := m["add"]; ok {
			var a deltaAdd
			if err := json.Unmarshal(raw, &a); err != nil {
				return c, fmt.Errorf("delta add: %w", err)
			}
			c.adds = append(c.adds, a)
		}
		if raw, ok := m["remove"]; ok {
			var r deltaRemove
			if err := json.Unmarshal(raw, &r); err != nil {
				return c, fmt.Errorf("delta remove: %w", err)
			}
			c.removes = append(c.removes, r)
		}
	}
	return c, nil
}

// columnTypeForDelta is the inverse of deltaType: a Delta/Spark type name → our
// Schema column type.
func columnTypeForDelta(t string) string {
	switch t {
	case "long":
		return "int64"
	case "double":
		return "float64"
	case "boolean":
		return "bool"
	case "timestamp":
		return "timestamp"
	default: // "string" and anything unknown
		return "string"
	}
}

// parseDeltaSchemaString decodes a Delta schemaString back into our Schema.
func parseDeltaSchemaString(s string) (Schema, error) {
	var st struct {
		Fields []struct {
			Name     string `json:"name"`
			Type     string `json:"type"`
			Nullable bool   `json:"nullable"`
		} `json:"fields"`
	}
	if err := json.Unmarshal([]byte(s), &st); err != nil {
		return Schema{}, fmt.Errorf("delta schemaString: %w", err)
	}
	sc := Schema{Version: 1, Columns: make([]Column, 0, len(st.Fields))}
	for _, f := range st.Fields {
		sc.Columns = append(sc.Columns, Column{Name: f.Name, Type: columnTypeForDelta(f.Type), Nullable: f.Nullable})
	}
	return sc, nil
}
