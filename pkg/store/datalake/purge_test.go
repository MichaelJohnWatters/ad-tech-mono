package datalake

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/fs"
)

func newTestStore(t *testing.T) *ObjectStore {
	t.Helper()
	obj, err := fs.New(t.TempDir())
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	const bucket = "lake"
	if err := obj.EnsureBucket(context.Background(), bucket); err != nil {
		t.Fatalf("ensure bucket: %v", err)
	}
	return NewObjectStore(obj, bucket, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestPurgeRowsFilteredRewrite(t *testing.T) {
	ctx := context.Background()
	o := newTestStore(t)
	schema := Schema{Version: 1, Columns: []Column{
		{Name: "user_id", Type: "string", Nullable: true},
		{Name: "kind", Type: "string", Nullable: true},
	}}
	// Two files so the rewrite spans the active set.
	if err := o.Write(ctx, "t", []Record{
		{"user_id": "keep-1", "kind": "request"},
		{"user_id": "purge-me", "kind": "request"},
	}, schema); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	if err := o.Write(ctx, "t", []Record{
		{"user_id": "purge-me", "kind": "click"},
		{"user_id": "keep-2", "kind": "request"},
	}, schema); err != nil {
		t.Fatalf("write 2: %v", err)
	}

	match := func(r Record) bool { return r["user_id"] == "purge-me" }
	if n, err := o.CountRows(ctx, "t", match); err != nil || n != 2 {
		t.Fatalf("pre-purge count = %d (%v), want 2", n, err)
	}
	removed, err := o.PurgeRows(ctx, "t", match)
	if err != nil {
		t.Fatalf("PurgeRows: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2", removed)
	}
	if n, _ := o.CountRows(ctx, "t", match); n != 0 {
		t.Errorf("post-purge residual = %d, want 0", n)
	}
	// Survivors intact, exactly once (single atomic commit — no double count).
	rows, err := o.Read(ctx, "t", Filter{})
	if err != nil {
		t.Fatalf("read after purge: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("surviving rows = %d, want 2", len(rows))
	}
	// Idempotent no-op second time (no new commit).
	txnsBefore, _ := o.Log(ctx, "t")
	if n, err := o.PurgeRows(ctx, "t", match); err != nil || n != 0 {
		t.Fatalf("second purge = %d (%v), want 0", n, err)
	}
	txnsAfter, _ := o.Log(ctx, "t")
	if len(txnsAfter) != len(txnsBefore) {
		t.Errorf("no-op purge wrote a commit (%d → %d)", len(txnsBefore), len(txnsAfter))
	}
}

// TestVacuumReclaimsPurgedBytes proves the GDPR tail: PurgeRows tombstones
// the pre-purge files but their BYTES remain in the bucket until Vacuum
// physically deletes them. Grace-window semantics: recent tombstones
// survive, expired ones go, active files are never touched.
func TestVacuumReclaimsPurgedBytes(t *testing.T) {
	ctx := context.Background()
	o := newTestStore(t)
	schema := Schema{Version: 1, Columns: []Column{{Name: "user_id", Type: "string", Nullable: true}}}
	if err := o.Write(ctx, "t", []Record{{"user_id": "victim"}, {"user_id": "keeper"}}, schema); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := o.PurgeRows(ctx, "t", func(r Record) bool { return r["user_id"] == "victim" }); err != nil {
		t.Fatalf("purge: %v", err)
	}

	// The tombstoned pre-purge file still physically exists.
	tombstoned := tombstonedPaths(t, o, "t")
	if len(tombstoned) == 0 {
		t.Fatal("purge left no tombstoned files (test premise broken)")
	}
	for _, p := range tombstoned {
		if ok, _ := o.obj.Exists(ctx, o.bucket, p); !ok {
			t.Fatalf("tombstoned file %s already gone before vacuum", p)
		}
	}

	// A wide grace window deletes nothing.
	res, err := o.Vacuum(ctx, "t", time.Hour)
	if err != nil {
		t.Fatalf("vacuum(1h): %v", err)
	}
	if res.FilesDeleted != 0 {
		t.Errorf("vacuum inside grace deleted %d files, want 0", res.FilesDeleted)
	}

	// Grace 0 physically deletes the tombstoned bytes...
	res, err = o.Vacuum(ctx, "t", 0)
	if err != nil {
		t.Fatalf("vacuum(0): %v", err)
	}
	if res.FilesDeleted != len(tombstoned) {
		t.Errorf("vacuum deleted %d files, want %d", res.FilesDeleted, len(tombstoned))
	}
	for _, p := range tombstoned {
		if ok, _ := o.obj.Exists(ctx, o.bucket, p); ok {
			t.Errorf("tombstoned file %s still exists after vacuum — purged bytes persist", p)
		}
	}
	// ...while the survivor's data stays fully readable.
	rows, err := o.Read(ctx, "t", Filter{})
	if err != nil {
		t.Fatalf("read after vacuum: %v", err)
	}
	if len(rows) != 1 || rows[0]["user_id"] != "keeper" {
		t.Errorf("post-vacuum rows = %v, want just keeper", rows)
	}
}

// tombstonedPaths lists files with a remove action and no active add.
func tombstonedPaths(t *testing.T, o *ObjectStore, table string) []string {
	t.Helper()
	txns, err := o.Log(context.Background(), table)
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	active := map[string]bool{}
	removed := map[string]bool{}
	for _, tx := range txns {
		switch tx.Action {
		case "add":
			active[tx.Path] = true
		case "remove":
			delete(active, tx.Path)
			removed[tx.Path] = true
		}
	}
	var out []string
	for p := range removed {
		if !active[p] {
			out = append(out, p)
		}
	}
	return out
}

func TestPurgeRowsRemovesWholeTable(t *testing.T) {
	ctx := context.Background()
	o := newTestStore(t)
	schema := Schema{Version: 1, Columns: []Column{{Name: "user_id", Type: "string", Nullable: true}}}
	if err := o.Write(ctx, "t", []Record{{"user_id": "u1"}, {"user_id": "u2"}}, schema); err != nil {
		t.Fatalf("write: %v", err)
	}
	removed, err := o.PurgeRows(ctx, "t", func(Record) bool { return true })
	if err != nil {
		t.Fatalf("PurgeRows: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2", removed)
	}
	rows, err := o.Read(ctx, "t", Filter{})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("rows after full purge = %d, want 0", len(rows))
	}
}
