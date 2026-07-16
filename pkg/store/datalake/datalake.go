// Package datalake provides Parquet read/write and Delta Log management
// for the data pipeline.
//
// Delta Log gives ACID transactions on top of object storage (Minio/S3).
// Every write creates a new Parquet file and appends a transaction entry
// to the log. This enables schema evolution, time travel, and rollback.
//
// Usage:
//
//	dl := datalake.New(objectStore, logger)
//	dl.Write(ctx, "normalised/impressions", records, schema)
//	records, err := dl.Read(ctx, "normalised/impressions", filter)
//	dl.Commit(ctx, "normalised/impressions", txn)
package datalake

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Store is the datalake interface for reading and writing Parquet data
// with Delta Log transaction management.
type Store interface {
	// Write appends records to a table, creating a new Parquet file.
	Write(ctx context.Context, table string, records []Record, schema Schema) error

	// Read returns all records from a table matching the filter.
	Read(ctx context.Context, table string, filter Filter) ([]Record, error)

	// Log returns the transaction log for a table.
	Log(ctx context.Context, table string) ([]Transaction, error)

	// Snapshot returns the current state of a table (all active files).
	Snapshot(ctx context.Context, table string) (*TableSnapshot, error)

	// PurgeRows rewrites the table without the rows matching match, in one
	// atomic Delta commit (GDPR deletion). Returns how many rows were removed.
	PurgeRows(ctx context.Context, table string, match func(Record) bool) (int, error)

	// CountRows counts rows matching match across the active file set
	// (read-only — the purge-verification counterpart of PurgeRows).
	CountRows(ctx context.Context, table string, match func(Record) bool) (int, error)

	// Compact bin-packs the table's active files into one (small-files fix).
	Compact(ctx context.Context, table string) (CompactResult, error)

	// Vacuum physically deletes tombstoned files older than grace — without
	// it, PurgeRows-removed (GDPR) bytes persist in the bucket forever.
	Vacuum(ctx context.Context, table string, grace time.Duration) (VacuumResult, error)

	// Close releases resources.
	Close() error
}

// Record is a row of data as typed key-value pairs.
type Record map[string]interface{}

// Schema defines the columns and types for a table.
type Schema struct {
	Version int
	Columns []Column
}

// Column defines a single column in a schema.
type Column struct {
	Name     string
	Type     string // string, int64, float64, bool, timestamp
	Nullable bool
}

// Filter constrains which records to read.
type Filter struct {
	TimeFrom time.Time
	TimeTo   time.Time
	Columns  map[string]interface{} // exact match filters
}

// Transaction is a single entry in the Delta Log.
type Transaction struct {
	Version   int       `json:"version"`
	Timestamp time.Time `json:"timestamp"`
	Action    string    `json:"action"` // add, remove, schema_change
	Path      string    `json:"path"`   // path to the Parquet file
	NumRows   int       `json:"num_rows"`
	ByteSize  int64     `json:"byte_size"`
	Schema    *Schema   `json:"schema,omitempty"` // only for schema_change
}

// TableSnapshot is the current state of a table.
type TableSnapshot struct {
	Table        string
	Version      int
	Schema       Schema
	ActiveFiles  []string
	TotalRows    int
	TotalBytes   int64
	LastModified time.Time
}

// MemoryStore is an in-memory datalake for testing.
type MemoryStore struct {
	mu     sync.RWMutex
	tables map[string]*memTable
	log    *slog.Logger
}

type memTable struct {
	records      []Record
	schema       Schema
	transactions []Transaction
	version      int
}

// NewMemory creates an in-memory datalake store.
func NewMemory(log *slog.Logger) *MemoryStore {
	return &MemoryStore{
		tables: make(map[string]*memTable),
		log:    log,
	}
}

func (m *MemoryStore) Write(_ context.Context, table string, records []Record, schema Schema) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	t, ok := m.tables[table]
	if !ok {
		t = &memTable{schema: schema}
		m.tables[table] = t
	}

	t.version++
	path := fmt.Sprintf("%s/part-%05d.parquet", table, t.version)

	t.records = append(t.records, records...)
	t.transactions = append(t.transactions, Transaction{
		Version:   t.version,
		Timestamp: time.Now().UTC(),
		Action:    "add",
		Path:      path,
		NumRows:   len(records),
	})

	m.log.Debug("datalake write",
		"table", table,
		"records", len(records),
		"version", t.version,
	)

	return nil
}

func (m *MemoryStore) Read(_ context.Context, table string, filter Filter) ([]Record, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	t, ok := m.tables[table]
	if !ok {
		return nil, nil
	}

	var result []Record
	for _, rec := range t.records {
		if matchRecord(rec, filter) {
			result = append(result, rec)
		}
	}
	return result, nil
}

func (m *MemoryStore) Log(_ context.Context, table string) ([]Transaction, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	t, ok := m.tables[table]
	if !ok {
		return nil, nil
	}

	out := make([]Transaction, len(t.transactions))
	copy(out, t.transactions)
	return out, nil
}

func (m *MemoryStore) Snapshot(_ context.Context, table string) (*TableSnapshot, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	t, ok := m.tables[table]
	if !ok {
		return nil, fmt.Errorf("table %s not found", table)
	}

	var files []string
	var totalBytes int64
	for _, txn := range t.transactions {
		if txn.Action == "add" {
			files = append(files, txn.Path)
			totalBytes += txn.ByteSize
		}
	}

	var lastMod time.Time
	if len(t.transactions) > 0 {
		lastMod = t.transactions[len(t.transactions)-1].Timestamp
	}

	return &TableSnapshot{
		Table:        table,
		Version:      t.version,
		Schema:       t.schema,
		ActiveFiles:  files,
		TotalRows:    len(t.records),
		TotalBytes:   totalBytes,
		LastModified: lastMod,
	}, nil
}

func (m *MemoryStore) PurgeRows(_ context.Context, table string, match func(Record) bool) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tables[table]
	if !ok {
		return 0, nil
	}
	kept := t.records[:0]
	removed := 0
	for _, rec := range t.records {
		if match(rec) {
			removed++
		} else {
			kept = append(kept, rec)
		}
	}
	t.records = kept
	return removed, nil
}

func (m *MemoryStore) CountRows(_ context.Context, table string, match func(Record) bool) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.tables[table]
	if !ok {
		return 0, nil
	}
	n := 0
	for _, rec := range t.records {
		if match(rec) {
			n++
		}
	}
	return n, nil
}

// Vacuum is a no-op for the in-memory store (rows are gone when purged).
func (m *MemoryStore) Vacuum(_ context.Context, table string, _ time.Duration) (VacuumResult, error) {
	return VacuumResult{Table: table}, nil
}

// Compact is a no-op for the in-memory store (no files to pack).
func (m *MemoryStore) Compact(_ context.Context, table string) (CompactResult, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	res := CompactResult{Table: table}
	if t, ok := m.tables[table]; ok {
		res.Rows = len(t.records)
	}
	return res, nil
}

func (m *MemoryStore) Close() error { return nil }

// Tables returns all table names for test assertions.
func (m *MemoryStore) Tables() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var names []string
	for k := range m.tables {
		names = append(names, k)
	}
	return names
}

func matchRecord(rec Record, filter Filter) bool {
	// Time filter
	if !filter.TimeFrom.IsZero() || !filter.TimeTo.IsZero() {
		if ts, ok := rec["timestamp"]; ok {
			var t time.Time
			switch v := ts.(type) {
			case time.Time:
				t = v
			case string:
				t, _ = time.Parse(time.RFC3339, v)
			}
			if !filter.TimeFrom.IsZero() && t.Before(filter.TimeFrom) {
				return false
			}
			if !filter.TimeTo.IsZero() && t.After(filter.TimeTo) {
				return false
			}
		}
	}

	// Column filters
	for k, v := range filter.Columns {
		rv, ok := rec[k]
		if !ok {
			return false
		}
		// Compare as JSON for type-agnostic equality
		a, _ := json.Marshal(rv)
		b, _ := json.Marshal(v)
		if string(a) != string(b) {
			return false
		}
	}

	return true
}
