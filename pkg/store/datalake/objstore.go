package datalake

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects"
)

// ObjectStore is the real datalake: it writes records as Apache Parquet
// files to object storage (Minio/S3 via objects.Store) and records each
// write as a JSON transaction in a Delta-style log under
// {table}/_delta_log/. Reads replay the log to find the active set of
// Parquet files, then decode them back to Records. This is the durable
// counterpart to MemoryStore (which only kept slices + fake paths).
//
// Layout in the bucket:
//
//	{table}/_delta_log/{version:020d}.json   one transaction per file
//	{table}/part-{version:05d}.parquet       one data file per write
//
// Parquet is self-describing, so Read recovers the schema from the file —
// the datalake.Schema is only consulted on Write to build the Arrow schema.
type ObjectStore struct {
	obj    objects.Store
	bucket string
	log    *slog.Logger
	mem    memory.Allocator

	// mu serialises version allocation within a process so two concurrent
	// writes to the same table don't pick the same version number. (Cross
	// -process coordination would need a real Delta commit protocol; this
	// matches the single-writer pipeline model.)
	mu sync.Mutex
}

// NewObjectStore returns a datalake backed by the given object store and
// bucket. EnsureBucket is the caller's responsibility (pipeline does it at
// boot).
func NewObjectStore(obj objects.Store, bucket string, log *slog.Logger) *ObjectStore {
	return &ObjectStore{obj: obj, bucket: bucket, log: log, mem: memory.NewGoAllocator()}
}

func deltaLogPrefix(table string) string { return table + "/_delta_log/" }
func deltaLogKey(table string, v int) string {
	return fmt.Sprintf("%s%020d.json", deltaLogPrefix(table), v)
}
func parquetKey(table string, v int) string {
	return fmt.Sprintf("%s/part-%05d.parquet", table, v)
}

func (o *ObjectStore) Write(ctx context.Context, table string, records []Record, schema Schema) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	// Next version = number of existing log entries (0-based monotonic).
	existing, err := o.obj.List(ctx, o.bucket, deltaLogPrefix(table))
	if err != nil {
		return fmt.Errorf("list delta log: %w", err)
	}
	version := len(existing)

	// Encode the records as Parquet.
	parquetBytes, err := o.encodeParquet(records, schema)
	if err != nil {
		return fmt.Errorf("encode parquet: %w", err)
	}
	pKey := parquetKey(table, version)
	if err := o.obj.Put(ctx, o.bucket, pKey, bytes.NewReader(parquetBytes), int64(len(parquetBytes)), "application/vnd.apache.parquet"); err != nil {
		return fmt.Errorf("put parquet: %w", err)
	}

	// Append the transaction to the Delta log. The first write also records
	// the schema so a reader can inspect the table without opening a file.
	txn := Transaction{
		Version:   version,
		Timestamp: time.Now().UTC(),
		Action:    "add",
		Path:      pKey,
		NumRows:   len(records),
		ByteSize:  int64(len(parquetBytes)),
	}
	if version == 0 {
		s := schema
		txn.Schema = &s
	}
	txnBytes, err := json.Marshal(txn)
	if err != nil {
		return fmt.Errorf("marshal txn: %w", err)
	}
	if err := o.obj.Put(ctx, o.bucket, deltaLogKey(table, version), bytes.NewReader(txnBytes), int64(len(txnBytes)), "application/json"); err != nil {
		return fmt.Errorf("put delta log: %w", err)
	}

	o.log.Debug("datalake write", "table", table, "records", len(records), "version", version, "bytes", len(parquetBytes))
	return nil
}

func (o *ObjectStore) Read(ctx context.Context, table string, filter Filter) ([]Record, error) {
	txns, err := o.Log(ctx, table)
	if err != nil {
		return nil, err
	}
	// Replay the log to the active file set (add then remove).
	active := map[string]bool{}
	for _, t := range txns {
		switch t.Action {
		case "add":
			active[t.Path] = true
		case "remove":
			delete(active, t.Path)
		}
	}
	paths := make([]string, 0, len(active))
	for p := range active {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var out []Record
	for _, p := range paths {
		recs, err := o.readParquet(ctx, p)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
		for _, rec := range recs {
			if matchRecord(rec, filter) {
				out = append(out, rec)
			}
		}
	}
	return out, nil
}

func (o *ObjectStore) Log(ctx context.Context, table string) ([]Transaction, error) {
	keys, err := o.obj.List(ctx, o.bucket, deltaLogPrefix(table))
	if err != nil {
		return nil, fmt.Errorf("list delta log: %w", err)
	}
	sort.Strings(keys) // version-ordered: zero-padded names sort lexically
	out := make([]Transaction, 0, len(keys))
	for _, k := range keys {
		if !strings.HasSuffix(k, ".json") {
			continue
		}
		body, err := o.getAll(ctx, k)
		if err != nil {
			return nil, fmt.Errorf("get %s: %w", k, err)
		}
		var t Transaction
		if err := json.Unmarshal(body, &t); err != nil {
			return nil, fmt.Errorf("unmarshal %s: %w", k, err)
		}
		out = append(out, t)
	}
	return out, nil
}

func (o *ObjectStore) Snapshot(ctx context.Context, table string) (*TableSnapshot, error) {
	txns, err := o.Log(ctx, table)
	if err != nil {
		return nil, err
	}
	if len(txns) == 0 {
		return nil, fmt.Errorf("table %s not found", table)
	}
	active := map[string]Transaction{}
	var schema Schema
	for _, t := range txns {
		if t.Schema != nil {
			schema = *t.Schema
		}
		switch t.Action {
		case "add":
			active[t.Path] = t
		case "remove":
			delete(active, t.Path)
		}
	}
	var files []string
	var rows int
	var bytesTotal int64
	for p, t := range active {
		files = append(files, p)
		rows += t.NumRows
		bytesTotal += t.ByteSize
	}
	sort.Strings(files)
	return &TableSnapshot{
		Table:        table,
		Version:      txns[len(txns)-1].Version,
		Schema:       schema,
		ActiveFiles:  files,
		TotalRows:    rows,
		TotalBytes:   bytesTotal,
		LastModified: txns[len(txns)-1].Timestamp,
	}, nil
}

func (o *ObjectStore) Close() error { return nil }

func (o *ObjectStore) getAll(ctx context.Context, key string) ([]byte, error) {
	rc, err := o.obj.Get(ctx, o.bucket, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// --- Parquet encode/decode ---

func (o *ObjectStore) encodeParquet(records []Record, schema Schema) ([]byte, error) {
	arrowSchema, err := arrowSchemaFor(schema)
	if err != nil {
		return nil, err
	}
	b := array.NewRecordBuilder(o.mem, arrowSchema)
	defer b.Release()

	for _, rec := range records {
		for i, col := range schema.Columns {
			appendValue(b.Field(i), col, rec[col.Name])
		}
	}
	arrRec := b.NewRecord()
	defer arrRec.Release()

	tbl := array.NewTableFromRecords(arrowSchema, []arrow.Record{arrRec})
	defer tbl.Release()

	var buf bytes.Buffer
	chunk := int64(len(records))
	if chunk == 0 {
		chunk = 1
	}
	if err := pqarrow.WriteTable(tbl, &buf, chunk,
		parquet.NewWriterProperties(parquet.WithAllocator(o.mem)),
		pqarrow.DefaultWriterProps()); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (o *ObjectStore) readParquet(ctx context.Context, key string) ([]Record, error) {
	raw, err := o.getAll(ctx, key)
	if err != nil {
		return nil, err
	}
	tbl, err := pqarrow.ReadTable(ctx, bytes.NewReader(raw),
		parquet.NewReaderProperties(o.mem), pqarrow.ArrowReadProperties{}, o.mem)
	if err != nil {
		return nil, err
	}
	defer tbl.Release()

	nrows := int(tbl.NumRows())
	out := make([]Record, nrows)
	for i := range out {
		out[i] = Record{}
	}
	for c := 0; c < int(tbl.NumCols()); c++ {
		col := tbl.Column(c)
		name := col.Name()
		row := 0
		for _, chunk := range col.Data().Chunks() {
			for j := 0; j < chunk.Len(); j++ {
				if row < nrows {
					out[row][name] = readValue(chunk, j)
				}
				row++
			}
		}
	}
	return out, nil
}

func arrowSchemaFor(schema Schema) (*arrow.Schema, error) {
	fields := make([]arrow.Field, len(schema.Columns))
	for i, c := range schema.Columns {
		dt, err := arrowType(c.Type)
		if err != nil {
			return nil, err
		}
		fields[i] = arrow.Field{Name: c.Name, Type: dt, Nullable: c.Nullable}
	}
	return arrow.NewSchema(fields, nil), nil
}

func arrowType(t string) (arrow.DataType, error) {
	switch t {
	case "string":
		return arrow.BinaryTypes.String, nil
	case "int64", "int":
		return arrow.PrimitiveTypes.Int64, nil
	case "float64", "float":
		return arrow.PrimitiveTypes.Float64, nil
	case "bool":
		return arrow.FixedWidthTypes.Boolean, nil
	case "timestamp":
		return arrow.FixedWidthTypes.Timestamp_us, nil
	default:
		return nil, fmt.Errorf("unsupported column type %q", t)
	}
}

func appendValue(b array.Builder, col Column, v interface{}) {
	if v == nil {
		b.AppendNull()
		return
	}
	switch bld := b.(type) {
	case *array.StringBuilder:
		bld.Append(toString(v))
	case *array.Int64Builder:
		bld.Append(toInt64(v))
	case *array.Float64Builder:
		bld.Append(toFloat64(v))
	case *array.BooleanBuilder:
		bld.Append(toBool(v))
	case *array.TimestampBuilder:
		bld.Append(arrow.Timestamp(toTime(v).UnixMicro()))
	default:
		b.AppendNull()
	}
}

func readValue(arr arrow.Array, i int) interface{} {
	if arr.IsNull(i) {
		return nil
	}
	switch a := arr.(type) {
	case *array.String:
		return a.Value(i)
	case *array.Int64:
		return a.Value(i)
	case *array.Float64:
		return a.Value(i)
	case *array.Boolean:
		return a.Value(i)
	case *array.Timestamp:
		return a.Value(i).ToTime(arrow.Microsecond).UTC()
	default:
		return nil
	}
}

func toString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func toInt64(v interface{}) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case int32:
		return int64(n)
	case float64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	default:
		return 0
	}
}

func toFloat64(v interface{}) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int64:
		return float64(n)
	case int:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	default:
		return 0
	}
}

func toBool(v interface{}) bool {
	b, _ := v.(bool)
	return b
}

func toTime(v interface{}) time.Time {
	switch t := v.(type) {
	case time.Time:
		return t.UTC()
	case string:
		parsed, _ := time.Parse(time.RFC3339, t)
		return parsed.UTC()
	default:
		return time.Time{}
	}
}
