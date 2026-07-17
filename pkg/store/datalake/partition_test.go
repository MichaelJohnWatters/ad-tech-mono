package datalake

// Partitioned-layout tests: hive-dir writes, read-time partition pruning
// (proven by counting object GETs — a pruned day is never fetched),
// per-partition compaction (old days immutable), purge preserving layout,
// and the one-time EnsurePartitioned migration of a flat table.

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/fs"
)

// countingStore wraps an objects.Store and counts Get calls per key, so
// tests can prove a file was pruned (never fetched), not just filtered.
type countingStore struct {
	objects.Store
	gets map[string]int
}

func (c *countingStore) Get(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	c.gets[key]++
	return c.Store.Get(ctx, bucket, key)
}

func newPartitionedLake(t *testing.T) (*ObjectStore, *countingStore) {
	t.Helper()
	base, err := fs.New(t.TempDir())
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	ctx := context.Background()
	if err := base.EnsureBucket(ctx, "datalake"); err != nil {
		t.Fatalf("ensure bucket: %v", err)
	}
	counting := &countingStore{Store: base, gets: map[string]int{}}
	return NewObjectStore(counting, "datalake", logger.New("datalake-test")), counting
}

var partitionedSchema = Schema{
	Version: 1,
	Columns: []Column{
		{Name: "user_id", Type: "string", Nullable: true},
		{Name: "kind", Type: "string", Nullable: true},
		{Name: "observed_at", Type: "timestamp", Nullable: true},
	},
	PartitionBy: "observed_at",
}

func day(d int, hour int) time.Time {
	return time.Date(2026, 7, d, hour, 0, 0, 0, time.UTC)
}

func behaviourRow(user string, at time.Time) Record {
	return Record{"user_id": user, "kind": "site_visit", "observed_at": at}
}

func TestPartitionedWrite_HiveLayoutAndRoundTrip(t *testing.T) {
	dl, obj := newPartitionedLake(t)
	ctx := context.Background()

	// One write spanning two days → two files, each in its day's dir,
	// committed in ONE Delta version.
	rows := []Record{
		behaviourRow("u1", day(10, 9)),
		behaviourRow("u2", day(10, 15)),
		behaviourRow("u3", day(11, 8)),
	}
	if err := dl.Write(ctx, "behaviour_signals", rows, partitionedSchema); err != nil {
		t.Fatalf("write: %v", err)
	}
	keys, err := obj.List(ctx, "datalake", "behaviour_signals/")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var d10, d11 bool
	for _, k := range keys {
		if strings.Contains(k, "event_date=2026-07-10/") {
			d10 = true
		}
		if strings.Contains(k, "event_date=2026-07-11/") {
			d11 = true
		}
	}
	if !d10 || !d11 {
		t.Fatalf("expected files under both day dirs, keys=%v", keys)
	}
	snap, err := dl.Snapshot(ctx, "behaviour_signals")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.Version != 0 || len(snap.ActiveFiles) != 2 || snap.TotalRows != 3 {
		t.Fatalf("snapshot = v%d files=%d rows=%d, want v0/2/3", snap.Version, len(snap.ActiveFiles), snap.TotalRows)
	}

	got, err := dl.Read(ctx, "behaviour_signals", Filter{})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("read %d rows, want 3", len(got))
	}
}

func TestPartitionedRead_PrunesOutOfWindowFiles(t *testing.T) {
	dl, obj := newPartitionedLake(t)
	ctx := context.Background()

	for d := 10; d <= 14; d++ {
		if err := dl.Write(ctx, "behaviour_signals", []Record{behaviourRow("u1", day(d, 12))}, partitionedSchema); err != nil {
			t.Fatalf("write day %d: %v", d, err)
		}
	}
	// Window = days 13-14. The days 10-12 files must never be FETCHED.
	for k := range obj.gets {
		delete(obj.gets, k)
	}
	got, err := dl.Read(ctx, "behaviour_signals", Filter{TimeFrom: day(13, 0)})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("windowed read = %d rows, want 2", len(got))
	}
	for k, n := range obj.gets {
		if strings.HasSuffix(k, ".parquet") && n > 0 {
			for _, old := range []string{"2026-07-10", "2026-07-11", "2026-07-12"} {
				if strings.Contains(k, old) {
					t.Errorf("out-of-window file was fetched: %s", k)
				}
			}
		}
	}

	// TimeTo prunes the other direction.
	got, err = dl.Read(ctx, "behaviour_signals", Filter{TimeTo: day(11, 23)})
	if err != nil {
		t.Fatalf("read to: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("TimeTo read = %d rows, want 2 (days 10-11)", len(got))
	}
}

func TestPartitionedCompact_PerPartitionAndOldDaysUntouched(t *testing.T) {
	dl, _ := newPartitionedLake(t)
	ctx := context.Background()

	// Day 10: three small files. Day 11: one file (already consolidated).
	for i := 0; i < 3; i++ {
		if err := dl.Write(ctx, "t", []Record{behaviourRow("u1", day(10, i+1))}, partitionedSchema); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := dl.Write(ctx, "t", []Record{behaviourRow("u2", day(11, 1))}, partitionedSchema); err != nil {
		t.Fatalf("write: %v", err)
	}
	before, _ := dl.Snapshot(ctx, "t")
	day11Before := ""
	for _, f := range before.ActiveFiles {
		if strings.Contains(f, "2026-07-11") {
			day11Before = f
		}
	}

	res, err := dl.Compact(ctx, "t")
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if res.FilesBefore != 4 || res.FilesAfter != 2 {
		t.Fatalf("compact = %+v, want 4→2", res)
	}
	after, _ := dl.Snapshot(ctx, "t")
	if len(after.ActiveFiles) != 2 || after.TotalRows != 4 {
		t.Fatalf("post-compact files=%d rows=%d, want 2/4", len(after.ActiveFiles), after.TotalRows)
	}
	// The single-file day was NOT rewritten — same path, immutable old day.
	found := false
	for _, f := range after.ActiveFiles {
		if f == day11Before {
			found = true
		}
	}
	if !found {
		t.Errorf("day-11 file was rewritten by compact; before=%s after=%v", day11Before, after.ActiveFiles)
	}

	// Idempotent: everything at 1 file per partition → no-op, no new commit.
	v := after.Version
	res2, err := dl.Compact(ctx, "t")
	if err != nil {
		t.Fatalf("compact 2: %v", err)
	}
	after2, _ := dl.Snapshot(ctx, "t")
	if res2.FilesBefore != 2 || res2.FilesAfter != 2 || after2.Version != v {
		t.Errorf("second compact = %+v (v%d→v%d), want no-op", res2, v, after2.Version)
	}
}

func TestPartitionedPurge_RewritesOnlyTouchedDays(t *testing.T) {
	dl, _ := newPartitionedLake(t)
	ctx := context.Background()

	// u1 appears on day 10 only; day 11 holds u2 only.
	if err := dl.Write(ctx, "t", []Record{behaviourRow("u1", day(10, 9)), behaviourRow("u2", day(10, 10))}, partitionedSchema); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := dl.Write(ctx, "t", []Record{behaviourRow("u2", day(11, 9))}, partitionedSchema); err != nil {
		t.Fatalf("write: %v", err)
	}
	before, _ := dl.Snapshot(ctx, "t")
	day11Before := ""
	for _, f := range before.ActiveFiles {
		if strings.Contains(f, "2026-07-11") {
			day11Before = f
		}
	}

	n, err := dl.PurgeRows(ctx, "t", func(r Record) bool { return r["user_id"] == "u1" })
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Fatalf("purged %d rows, want 1", n)
	}
	after, _ := dl.Snapshot(ctx, "t")
	if len(after.ActiveFiles) != 2 || after.TotalRows != 2 {
		t.Fatalf("post-purge files=%d rows=%d, want 2/2", len(after.ActiveFiles), after.TotalRows)
	}
	// Day 11 (no matches) must be byte-identical untouched — same path.
	found := false
	for _, f := range after.ActiveFiles {
		if f == day11Before {
			found = true
		}
		if strings.Contains(f, "2026-07-10") && !strings.Contains(f, "part-00002") {
			t.Errorf("day-10 file not rewritten at the purge version: %s", f)
		}
	}
	if !found {
		t.Errorf("day-11 file was rewritten by purge of day-10 rows")
	}
	left, err := dl.CountRows(ctx, "t", func(r Record) bool { return r["user_id"] == "u1" })
	if err != nil || left != 0 {
		t.Fatalf("residual u1 rows = %d (err %v), want 0", left, err)
	}
}

func TestEnsurePartitioned_MigratesFlatTable(t *testing.T) {
	dl, obj := newPartitionedLake(t)
	ctx := context.Background()

	// Build the OLD world: a flat (unpartitioned) table spanning two days.
	flat := partitionedSchema
	flat.PartitionBy = ""
	if err := dl.Write(ctx, "t", []Record{behaviourRow("u1", day(10, 9)), behaviourRow("u2", day(11, 9))}, flat); err != nil {
		t.Fatalf("write flat: %v", err)
	}
	if err := dl.Write(ctx, "t", []Record{behaviourRow("u3", day(11, 10))}, flat); err != nil {
		t.Fatalf("write flat 2: %v", err)
	}

	if err := dl.EnsurePartitioned(ctx, "t", partitionedSchema); err != nil {
		t.Fatalf("ensure partitioned: %v", err)
	}
	snap, err := dl.Snapshot(ctx, "t")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(snap.ActiveFiles) != 2 || snap.TotalRows != 3 {
		t.Fatalf("post-migration files=%d rows=%d, want 2/3", len(snap.ActiveFiles), snap.TotalRows)
	}
	for _, f := range snap.ActiveFiles {
		if !strings.Contains(f, "event_date=") {
			t.Errorf("active file not in hive layout after migration: %s", f)
		}
	}
	got, err := dl.Read(ctx, "t", Filter{})
	if err != nil || len(got) != 3 {
		t.Fatalf("post-migration read = %d rows (err %v), want 3", len(got), err)
	}

	// Idempotent — a second call commits nothing.
	v := snap.Version
	if err := dl.EnsurePartitioned(ctx, "t", partitionedSchema); err != nil {
		t.Fatalf("ensure partitioned 2: %v", err)
	}
	snap2, _ := dl.Snapshot(ctx, "t")
	if snap2.Version != v {
		t.Errorf("second EnsurePartitioned committed v%d (was v%d), want no-op", snap2.Version, v)
	}

	// Subsequent writes land partitioned (metadata rewrite took).
	if err := dl.Write(ctx, "t", []Record{behaviourRow("u4", day(12, 9))}, partitionedSchema); err != nil {
		t.Fatalf("write after migration: %v", err)
	}
	snap3, _ := dl.Snapshot(ctx, "t")
	found := false
	for _, f := range snap3.ActiveFiles {
		if strings.Contains(f, "event_date=2026-07-12/") {
			found = true
		}
	}
	if !found {
		t.Errorf("post-migration write not partitioned: %v", snap3.ActiveFiles)
	}

	// Vacuum (grace 0) reclaims the flat files' bytes.
	if _, err := dl.Vacuum(ctx, "t", 0); err != nil {
		t.Fatalf("vacuum: %v", err)
	}
	keys, _ := obj.List(ctx, "datalake", "t/")
	for _, k := range keys {
		if strings.HasSuffix(k, ".parquet") && !strings.Contains(k, "event_date=") {
			t.Errorf("flat file survived vacuum: %s", k)
		}
	}

	// A windowed read on the migrated table prunes the migrated days too.
	for k := range obj.gets {
		delete(obj.gets, k)
	}
	if _, err := dl.Read(ctx, "t", Filter{TimeFrom: day(12, 0)}); err != nil {
		t.Fatalf("windowed read: %v", err)
	}
	for k, n := range obj.gets {
		if strings.HasSuffix(k, ".parquet") && n > 0 && (strings.Contains(k, "2026-07-10") || strings.Contains(k, "2026-07-11")) {
			t.Errorf("pre-window migrated file fetched: %s", k)
		}
	}
}

func TestEnsurePartitioned_SweepsStrayFlatFiles(t *testing.T) {
	dl, _ := newPartitionedLake(t)
	ctx := context.Background()

	// A partitioned table...
	if err := dl.Write(ctx, "t", []Record{behaviourRow("u1", day(10, 9))}, partitionedSchema); err != nil {
		t.Fatalf("write: %v", err)
	}
	// ...then a ROLLED-BACK old writer appends a flat file (unpartitioned
	// schema — the pre-partitioning binary's behaviour).
	flat := partitionedSchema
	flat.PartitionBy = ""
	if err := dl.Write(ctx, "t", []Record{behaviourRow("u2", day(11, 9))}, flat); err != nil {
		t.Fatalf("write flat stray: %v", err)
	}
	snap, _ := dl.Snapshot(ctx, "t")
	partitionedBefore := ""
	for _, f := range snap.ActiveFiles {
		if strings.Contains(f, "event_date=") {
			partitionedBefore = f
		}
	}

	// The boot sweep repairs the stray without touching the good file.
	if err := dl.EnsurePartitioned(ctx, "t", partitionedSchema); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	after, _ := dl.Snapshot(ctx, "t")
	if after.TotalRows != 2 || len(after.ActiveFiles) != 2 {
		t.Fatalf("post-sweep files=%d rows=%d, want 2/2", len(after.ActiveFiles), after.TotalRows)
	}
	goodUntouched := false
	for _, f := range after.ActiveFiles {
		if !strings.Contains(f, "event_date=") {
			t.Errorf("stray survived the sweep: %s", f)
		}
		if f == partitionedBefore {
			goodUntouched = true
		}
	}
	if !goodUntouched {
		t.Error("properly-partitioned file was rewritten by the stray sweep")
	}
}

func TestPartitioned_NullPartitionNeverPruned(t *testing.T) {
	dl, obj := newPartitionedLake(t)
	ctx := context.Background()

	// A row with no observed_at lands in the hive null dir. File-level
	// pruning must be conservative: any time window still FETCHES the null
	// partition (row-level filtering then applies, as it always has).
	rows := []Record{
		{"user_id": "u-null", "kind": "site_visit"},
		behaviourRow("u1", day(10, 9)),
	}
	if err := dl.Write(ctx, "t", rows, partitionedSchema); err != nil {
		t.Fatalf("write: %v", err)
	}
	all, err := dl.Read(ctx, "t", Filter{})
	if err != nil || len(all) != 2 {
		t.Fatalf("full read = %d rows (err %v), want 2", len(all), err)
	}
	for k := range obj.gets {
		delete(obj.gets, k)
	}
	if _, err := dl.Read(ctx, "t", Filter{TimeFrom: day(20, 0)}); err != nil {
		t.Fatalf("windowed read: %v", err)
	}
	nullFetched := false
	for k, n := range obj.gets {
		if !strings.HasSuffix(k, ".parquet") || n == 0 {
			continue
		}
		if strings.Contains(k, hiveNullPartition) {
			nullFetched = true
		}
		if strings.Contains(k, "2026-07-10") {
			t.Errorf("out-of-window day file fetched: %s", k)
		}
	}
	if !nullFetched {
		t.Error("null-partition file was pruned — pruning must be conservative")
	}
}
