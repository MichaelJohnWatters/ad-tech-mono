package datalake

import (
	"context"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
)

func TestMemoryStore_WriteAndRead(t *testing.T) {
	log := logger.New("datalake-test")
	store := NewMemory(log)
	ctx := context.Background()

	schema := Schema{
		Version: 1,
		Columns: []Column{
			{Name: "campaign_id", Type: "string"},
			{Name: "impressions", Type: "int64"},
			{Name: "geo", Type: "string"},
			{Name: "timestamp", Type: "timestamp"},
		},
	}

	records := []Record{
		{"campaign_id": "c1", "impressions": 100, "geo": "GBR", "timestamp": time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)},
		{"campaign_id": "c2", "impressions": 200, "geo": "USA", "timestamp": time.Date(2024, 6, 15, 11, 0, 0, 0, time.UTC)},
		{"campaign_id": "c1", "impressions": 150, "geo": "GBR", "timestamp": time.Date(2024, 6, 16, 10, 0, 0, 0, time.UTC)},
	}

	err := store.Write(ctx, "normalised/impressions", records, schema)
	if err != nil {
		t.Fatal(err)
	}

	// Read all
	all, err := store.Read(ctx, "normalised/impressions", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("expected 3 records, got %d", len(all))
	}

	// Read with column filter
	filtered, err := store.Read(ctx, "normalised/impressions", Filter{
		Columns: map[string]interface{}{"geo": "GBR"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 2 {
		t.Errorf("expected 2 GBR records, got %d", len(filtered))
	}

	// Read with time filter
	timeFiltered, err := store.Read(ctx, "normalised/impressions", Filter{
		TimeFrom: time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC),
		TimeTo:   time.Date(2024, 6, 15, 23, 59, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(timeFiltered) != 2 {
		t.Errorf("expected 2 records for June 15, got %d", len(timeFiltered))
	}
}

func TestMemoryStore_TransactionLog(t *testing.T) {
	log := logger.New("datalake-test")
	store := NewMemory(log)
	ctx := context.Background()
	schema := Schema{Version: 1}

	// Two writes = two transactions
	store.Write(ctx, "test_table", []Record{{"a": 1}}, schema)
	store.Write(ctx, "test_table", []Record{{"b": 2}, {"c": 3}}, schema)

	txns, err := store.Log(ctx, "test_table")
	if err != nil {
		t.Fatal(err)
	}
	if len(txns) != 2 {
		t.Fatalf("expected 2 transactions, got %d", len(txns))
	}
	if txns[0].Version != 1 || txns[1].Version != 2 {
		t.Errorf("versions = %d, %d, want 1, 2", txns[0].Version, txns[1].Version)
	}
	if txns[0].NumRows != 1 || txns[1].NumRows != 2 {
		t.Errorf("row counts = %d, %d, want 1, 2", txns[0].NumRows, txns[1].NumRows)
	}
}

func TestMemoryStore_Snapshot(t *testing.T) {
	log := logger.New("datalake-test")
	store := NewMemory(log)
	ctx := context.Background()
	schema := Schema{Version: 1, Columns: []Column{{Name: "x", Type: "int64"}}}

	store.Write(ctx, "tbl", []Record{{"x": 1}, {"x": 2}}, schema)
	store.Write(ctx, "tbl", []Record{{"x": 3}}, schema)

	snap, err := store.Snapshot(ctx, "tbl")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Version != 2 {
		t.Errorf("version = %d, want 2", snap.Version)
	}
	if snap.TotalRows != 3 {
		t.Errorf("total rows = %d, want 3", snap.TotalRows)
	}
	if len(snap.ActiveFiles) != 2 {
		t.Errorf("active files = %d, want 2", len(snap.ActiveFiles))
	}
}

func TestMemoryStore_EmptyTable(t *testing.T) {
	log := logger.New("datalake-test")
	store := NewMemory(log)
	ctx := context.Background()

	records, err := store.Read(ctx, "nonexistent", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if records != nil {
		t.Error("expected nil for nonexistent table")
	}

	_, err = store.Snapshot(ctx, "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent table snapshot")
	}
}

func TestMemoryStore_Tables(t *testing.T) {
	log := logger.New("datalake-test")
	store := NewMemory(log)
	ctx := context.Background()

	store.Write(ctx, "table_a", []Record{{"x": 1}}, Schema{})
	store.Write(ctx, "table_b", []Record{{"y": 2}}, Schema{})

	tables := store.Tables()
	if len(tables) != 2 {
		t.Errorf("expected 2 tables, got %d", len(tables))
	}
}

func TestStoreInterface(t *testing.T) {
	var _ Store = (*MemoryStore)(nil)
}
