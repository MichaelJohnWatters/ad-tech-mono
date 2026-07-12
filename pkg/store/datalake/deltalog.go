package datalake

import (
	"encoding/json"
	"strings"
)

// Real Delta Lake transaction-log encoding (protocol / metaData / add / remove
// actions, newline-delimited JSON) — as opposed to our home-grown
// one-Transaction-per-file log (see objstore.go). This is the building block for
// making the lake readable by STANDARD Delta readers (Spark, Trino, DuckDB
// delta_scan) instead of only our own reader.
//
// PROTOTYPE STATUS: this encoder is exercised by deltalog_test.go (shape) and
// deltalog_integration_test.go (a real Delta log wrapping our Parquet files, read
// back via delta_scan). It is NOT yet wired into ObjectStore.Write/Compact — that
// migration is the follow-up this prototype de-risks.

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
