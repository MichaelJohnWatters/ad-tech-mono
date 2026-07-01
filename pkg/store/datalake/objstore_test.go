package datalake

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/fs"
)

func newFSDatalake(t *testing.T) (*ObjectStore, *fs.Store) {
	t.Helper()
	obj, err := fs.New(t.TempDir())
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	ctx := context.Background()
	if err := obj.EnsureBucket(ctx, "datalake"); err != nil {
		t.Fatalf("ensure bucket: %v", err)
	}
	return NewObjectStore(obj, "datalake", logger.New("datalake-test")), obj
}

var testSchema = Schema{
	Version: 1,
	Columns: []Column{
		{Name: "campaign_id", Type: "string"},
		{Name: "impressions", Type: "int64"},
		{Name: "revenue", Type: "float64"},
		{Name: "viewable", Type: "bool", Nullable: true},
		{Name: "geo", Type: "string"},
		{Name: "timestamp", Type: "timestamp"},
	},
}

func TestObjectStore_WriteReadRoundTrip(t *testing.T) {
	dl, obj := newFSDatalake(t)
	ctx := context.Background()

	records := []Record{
		{"campaign_id": "c1", "impressions": int64(100), "revenue": 12.5, "viewable": true, "geo": "GBR", "timestamp": time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)},
		{"campaign_id": "c2", "impressions": int64(200), "revenue": 40.0, "viewable": false, "geo": "USA", "timestamp": time.Date(2024, 6, 15, 11, 0, 0, 0, time.UTC)},
	}
	if err := dl.Write(ctx, "normalised/impressions", records, testSchema); err != nil {
		t.Fatalf("write: %v", err)
	}

	// A real .parquet file must exist in the object store.
	keys, err := obj.List(ctx, "datalake", "normalised/impressions/")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var hasParquet, hasLog bool
	for _, k := range keys {
		if strings.HasSuffix(k, ".parquet") {
			hasParquet = true
		}
		if strings.Contains(k, "_delta_log/") && strings.HasSuffix(k, ".json") {
			hasLog = true
		}
	}
	if !hasParquet {
		t.Errorf("no .parquet file written; keys=%v", keys)
	}
	if !hasLog {
		t.Errorf("no _delta_log/*.json written; keys=%v", keys)
	}

	got, err := dl.Read(ctx, "normalised/impressions", Filter{})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("read %d records, want 2", len(got))
	}

	// Verify typed values round-trip through Parquet.
	byCampaign := map[string]Record{}
	for _, r := range got {
		byCampaign[r["campaign_id"].(string)] = r
	}
	c1 := byCampaign["c1"]
	if c1["impressions"].(int64) != 100 {
		t.Errorf("c1 impressions = %v, want 100", c1["impressions"])
	}
	if c1["revenue"].(float64) != 12.5 {
		t.Errorf("c1 revenue = %v, want 12.5", c1["revenue"])
	}
	if c1["viewable"].(bool) != true {
		t.Errorf("c1 viewable = %v, want true", c1["viewable"])
	}
	if ts, ok := c1["timestamp"].(time.Time); !ok || !ts.Equal(time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("c1 timestamp = %v, want 2024-06-15T10:00:00Z", c1["timestamp"])
	}
}

func TestObjectStore_FilterOnRead(t *testing.T) {
	dl, _ := newFSDatalake(t)
	ctx := context.Background()
	records := []Record{
		{"campaign_id": "c1", "impressions": int64(1), "revenue": 1.0, "viewable": true, "geo": "GBR", "timestamp": time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)},
		{"campaign_id": "c2", "impressions": int64(2), "revenue": 2.0, "viewable": true, "geo": "USA", "timestamp": time.Date(2024, 6, 15, 11, 0, 0, 0, time.UTC)},
	}
	if err := dl.Write(ctx, "t", records, testSchema); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := dl.Read(ctx, "t", Filter{Columns: map[string]interface{}{"geo": "USA"}})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 1 || got[0]["campaign_id"].(string) != "c2" {
		t.Fatalf("filtered read = %+v, want only c2", got)
	}
}

func TestObjectStore_DeltaLogAccumulatesAndSnapshot(t *testing.T) {
	dl, _ := newFSDatalake(t)
	ctx := context.Background()
	rec := []Record{{"campaign_id": "c1", "impressions": int64(1), "revenue": 1.0, "viewable": true, "geo": "GBR", "timestamp": time.Now().UTC()}}

	for i := 0; i < 3; i++ {
		if err := dl.Write(ctx, "t", rec, testSchema); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	txns, err := dl.Log(ctx, "t")
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	if len(txns) != 3 {
		t.Fatalf("delta log has %d entries, want 3", len(txns))
	}
	for i, txn := range txns {
		if txn.Version != i {
			t.Errorf("txn[%d].Version = %d, want %d", i, txn.Version, i)
		}
		if txn.Action != "add" {
			t.Errorf("txn[%d].Action = %s, want add", i, txn.Action)
		}
	}
	// Only the first txn carries the schema.
	if txns[0].Schema == nil {
		t.Error("first txn should record the schema")
	}

	snap, err := dl.Snapshot(ctx, "t")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(snap.ActiveFiles) != 3 {
		t.Errorf("snapshot active files = %d, want 3", len(snap.ActiveFiles))
	}
	if snap.TotalRows != 3 {
		t.Errorf("snapshot total rows = %d, want 3", snap.TotalRows)
	}
}

// Compile-time check that ObjectStore satisfies the Store interface.
var _ Store = (*ObjectStore)(nil)

// TestObjectStore_Compact proves file-level compaction: many small writes
// become one active file, row content is preserved, the old files are removed
// from the active set, and a second Compact is a no-op (idempotent).
func TestObjectStore_Compact(t *testing.T) {
	dl, _ := newFSDatalake(t)
	ctx := context.Background()

	// Five small single-row writes → five active Parquet files.
	for i := 0; i < 5; i++ {
		rec := []Record{{"campaign_id": "c1", "impressions": int64(10), "revenue": 1.5, "viewable": true, "geo": "GBR", "timestamp": time.Now().UTC()}}
		if err := dl.Write(ctx, "impressions", rec, testSchema); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	before, err := dl.Snapshot(ctx, "impressions")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(before.ActiveFiles) != 5 || before.TotalRows != 5 {
		t.Fatalf("pre-compact: files=%d rows=%d, want 5/5", len(before.ActiveFiles), before.TotalRows)
	}

	res, err := dl.Compact(ctx, "impressions")
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if res.FilesBefore != 5 || res.FilesAfter != 1 || res.Rows != 5 {
		t.Fatalf("compact result = %+v, want 5→1 / 5 rows", res)
	}

	after, err := dl.Snapshot(ctx, "impressions")
	if err != nil {
		t.Fatalf("snapshot after: %v", err)
	}
	if len(after.ActiveFiles) != 1 {
		t.Errorf("post-compact active files = %d, want 1", len(after.ActiveFiles))
	}
	if after.TotalRows != 5 {
		t.Errorf("post-compact rows = %d, want 5 (content preserved)", after.TotalRows)
	}
	// Read must still return all 5 rows (old files removed, not lost).
	recs, err := dl.Read(ctx, "impressions", Filter{})
	if err != nil {
		t.Fatalf("read after compact: %v", err)
	}
	if len(recs) != 5 {
		t.Errorf("read after compact = %d rows, want 5", len(recs))
	}

	// Idempotent: compacting a single-file table is a no-op.
	res2, err := dl.Compact(ctx, "impressions")
	if err != nil {
		t.Fatalf("compact 2: %v", err)
	}
	if res2.FilesBefore != 1 || res2.FilesAfter != 1 {
		t.Errorf("second compact = %+v, want no-op 1→1", res2)
	}
}
