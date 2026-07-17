package datalake

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
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
// files to object storage (Minio/S3 via objects.Store) and commits each write
// as a real Delta Lake transaction (protocol/metaData/add/remove actions) under
// {table}/_delta_log/. Reads replay the log to find the active set of Parquet
// files, then decode them back to Records. Because the log is standard Delta,
// external readers (Spark/Trino/DuckDB delta_scan) can read the lake too — see
// deltalog.go. This is the durable counterpart to MemoryStore (which only kept
// slices + fake paths).
//
// Layout in the bucket:
//
//	{table}/_delta_log/{version:020d}.json   one Delta commit per file
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

// hiveNullPartition is Hive/Delta's directory name for a null partition value
// (a record whose partition-source column is missing/zero). Standard readers
// surface it as event_date NULL; pruning never skips it.
const hiveNullPartition = "__HIVE_DEFAULT_PARTITION__"

// parquetKey names a data file. partition is the event_date value ("" for an
// unpartitioned table); seq disambiguates multiple files committed in one
// version (multi-day write, per-partition compaction, per-file purge rewrite).
func parquetKey(table string, v, seq int, partition string) string {
	name := fmt.Sprintf("part-%05d-%03d.parquet", v, seq)
	if partition == "" {
		return fmt.Sprintf("%s/%s", table, name)
	}
	return fmt.Sprintf("%s/%s=%s/%s", table, partitionColumn, partition, name)
}

// partitionValueFor derives a record's event_date partition value from the
// schema's partition-source column: the UTC date, or the hive null dir when
// the column is missing/unparseable.
func partitionValueFor(rec Record, sourceCol string) string {
	t := toTime(rec[sourceCol])
	if t.IsZero() {
		return hiveNullPartition
	}
	return t.UTC().Format("2006-01-02")
}

// groupByPartition splits records into per-partition groups with a stable
// (sorted) key order. Unpartitioned schemas yield a single ""-keyed group.
// Empty input still yields one empty group so a first Write always creates
// the table (version-0 metadata), matching the flat-layout behaviour.
func groupByPartition(records []Record, sourceCol string) (keys []string, groups map[string][]Record) {
	groups = map[string][]Record{}
	if sourceCol == "" {
		groups[""] = records
		return []string{""}, groups
	}
	for _, rec := range records {
		v := partitionValueFor(rec, sourceCol)
		groups[v] = append(groups[v], rec)
	}
	if len(groups) == 0 {
		groups[hiveNullPartition] = nil
	}
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, groups
}

// partitionValues renders the Delta add-action partitionValues map for a
// partition key. The hive null dir maps to the empty string (Delta's null
// partition-value serialisation).
func partitionValues(partition string) map[string]string {
	if partition == "" {
		return map[string]string{}
	}
	v := partition
	if v == hiveNullPartition {
		v = ""
	}
	return map[string]string{partitionColumn: v}
}

// partitionOfPath extracts the event_date value from a data-file path
// ("" , false when the path has no partition directory).
func partitionOfPath(path string) (string, bool) {
	for _, seg := range strings.Split(path, "/") {
		if v, ok := strings.CutPrefix(seg, partitionColumn+"="); ok {
			return v, true
		}
	}
	return "", false
}

// prunedByTime reports whether a data file can be skipped entirely for a
// time-bounded read: its partition date lies wholly outside [from, to].
// Files without a parseable partition date (unpartitioned tables, the hive
// null dir) are never pruned — pruning is an optimisation, row-level
// filtering still applies.
func prunedByTime(path string, from, to time.Time) bool {
	if from.IsZero() && to.IsZero() {
		return false
	}
	v, ok := partitionOfPath(path)
	if !ok {
		return false
	}
	day, err := time.Parse("2006-01-02", v)
	if err != nil {
		return false
	}
	dayEnd := day.AddDate(0, 0, 1)
	if !from.IsZero() && !dayEnd.After(from) {
		return true
	}
	if !to.IsZero() && day.After(to) {
		return true
	}
	return false
}

// deltaTableID is a stable table identifier for the Delta metaData action.
// (Standard Delta uses a UUID; readers treat it as an opaque string, so a
// deterministic per-table value is fine and keeps writes reproducible.)
func deltaTableID(table string) string { return "adtech:" + table }

// relTablePath converts a full object key (table/part-N.parquet) to the path
// Delta records in add/remove actions: relative to the table root.
func relTablePath(table, key string) string { return strings.TrimPrefix(key, table+"/") }

// versionFromLogKey parses the commit version out of a _delta_log/{v:020d}.json
// object key.
func versionFromLogKey(key string) (int, bool) {
	base := key
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSuffix(base, ".json")
	v, err := strconv.Atoi(base)
	if err != nil {
		return 0, false
	}
	return v, true
}

func (o *ObjectStore) Write(ctx context.Context, table string, records []Record, schema Schema) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	version, err := o.nextVersionLocked(ctx, table)
	if err != nil {
		return err
	}

	// One Parquet file per partition the records span (usually one — the
	// sink flushes minutely), all committed in ONE Delta transaction.
	// Version 0 carries protocol + metaData (schema, partitionColumns) so
	// standard Delta readers (Spark/Trino/DuckDB delta_scan) can open the
	// table; later versions are adds only.
	now := time.Now().UTC()
	keys, groups := groupByPartition(records, schema.PartitionBy)
	adds := make([]deltaAdd, 0, len(keys))
	totalBytes := 0
	for seq, part := range keys {
		group := groups[part]
		parquetBytes, err := o.encodeParquet(group, schema)
		if err != nil {
			return fmt.Errorf("encode parquet: %w", err)
		}
		pKey := parquetKey(table, version, seq, part)
		if err := o.obj.Put(ctx, o.bucket, pKey, bytes.NewReader(parquetBytes), int64(len(parquetBytes)), "application/vnd.apache.parquet"); err != nil {
			return fmt.Errorf("put parquet: %w", err)
		}
		totalBytes += len(parquetBytes)
		adds = append(adds, deltaAdd{
			Path:             relTablePath(table, pKey),
			PartitionValues:  partitionValues(part),
			Size:             int64(len(parquetBytes)),
			ModificationTime: now.UnixMilli(),
			DataChange:       true,
			Stats:            deltaStats(group, schema.PartitionBy),
		})
	}

	var meta *deltaMetaData
	if version == 0 {
		md, err := deltaMetaDataFor(deltaTableID(table), schema, now.UnixMilli())
		if err != nil {
			return fmt.Errorf("build delta metadata: %w", err)
		}
		meta = &md
	}
	commit, err := buildDeltaCommit(version == 0, meta, adds, nil)
	if err != nil {
		return fmt.Errorf("build delta commit: %w", err)
	}
	if err := o.putLogFile(ctx, table, version, commit); err != nil {
		return err
	}

	o.log.Debug("datalake write", "table", table, "records", len(records), "version", version, "files", len(adds), "bytes", totalBytes)
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
	pruned := 0
	for _, p := range paths {
		// Partition pruning: a time-bounded read never fetches a byte of a
		// day outside the window — the point of the hive layout.
		if prunedByTime(p, filter.TimeFrom, filter.TimeTo) {
			pruned++
			continue
		}
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
	if pruned > 0 {
		o.log.Debug("datalake read pruned partitions", "table", table, "files_pruned", pruned, "files_read", len(paths)-pruned)
	}
	return out, nil
}

// Log replays the Delta commit files into our internal Transaction model (one
// per add/remove action). Paths are re-expanded from Delta's table-relative form
// back to full object keys so Read/Compact can fetch the Parquet directly. The
// schema (recorded once, in version 0's metaData) rides on that commit's add.
func (o *ObjectStore) Log(ctx context.Context, table string) ([]Transaction, error) {
	keys, err := o.obj.List(ctx, o.bucket, deltaLogPrefix(table))
	if err != nil {
		return nil, fmt.Errorf("list delta log: %w", err)
	}
	sort.Strings(keys) // version-ordered: zero-padded names sort lexically
	var out []Transaction
	for _, k := range keys {
		if !strings.HasSuffix(k, ".json") {
			continue
		}
		version, ok := versionFromLogKey(k)
		if !ok {
			continue
		}
		body, err := o.getAll(ctx, k)
		if err != nil {
			return nil, fmt.Errorf("get %s: %w", k, err)
		}
		commit, err := parseDeltaCommit(body)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", k, err)
		}
		for _, a := range commit.adds {
			out = append(out, Transaction{
				Version:   version,
				Timestamp: time.UnixMilli(a.ModificationTime).UTC(),
				Action:    "add",
				Path:      table + "/" + a.Path,
				NumRows:   numRecordsFromStats(a.Stats),
				ByteSize:  a.Size,
				Schema:    commit.schema, // non-nil only for the commit that carried metaData
			})
		}
		for _, r := range commit.removes {
			out = append(out, Transaction{
				Version:   version,
				Timestamp: time.UnixMilli(r.DeletionTimestamp).UTC(),
				Action:    "remove",
				Path:      table + "/" + r.Path,
			})
		}
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

// CompactResult reports what a Compact pass did.
type CompactResult struct {
	Table       string
	FilesBefore int
	FilesAfter  int
	Rows        int
}

// Compact bin-packs a table's active Parquet files PER PARTITION, marking the
// superseded files removed in the Delta log — fixing the small-files problem
// that minutely flushes create. Only partitions holding 2+ files are touched,
// so with daily partitioning an old day compacts once and is then immutable
// forever: the hourly compactor only ever rewrites "today". Row content is
// unchanged (file-level compaction, not aggregation): a Read before and after
// returns the same records. Idempotent: with every partition already at ≤1
// file it's a no-op with no commit. Holds the write lock so it's atomic
// w.r.t. concurrent writes (single-writer model).
func (o *ObjectStore) Compact(ctx context.Context, table string) (CompactResult, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	txns, err := o.Log(ctx, table)
	if err != nil {
		return CompactResult{}, err
	}
	paths, schema, haveSchema := activeFiles(txns)
	res := CompactResult{Table: table, FilesBefore: len(paths), FilesAfter: len(paths)}
	if len(paths) <= 1 || !haveSchema {
		return res, nil // nothing to compact (or no schema recorded yet)
	}

	// Group the active set by partition value ("" = unpartitioned/flat files,
	// which all share one group — the old whole-table behaviour).
	byPart := map[string][]string{}
	for _, p := range paths {
		v, _ := partitionOfPath(p)
		byPart[v] = append(byPart[v], p)
	}
	parts := make([]string, 0, len(byPart))
	for v, files := range byPart {
		if len(files) > 1 {
			parts = append(parts, v)
		}
	}
	if len(parts) == 0 {
		return res, nil // every partition already consolidated
	}
	sort.Strings(parts)

	// Consolidate each multi-file partition, then commit ONE atomic Delta
	// transaction covering all of them. (A single commit means a reader never
	// sees a new file alongside the old ones — no double-count window.)
	version, err := o.nextVersionLocked(ctx, table)
	if err != nil {
		return res, err
	}
	now := time.Now().UTC()
	var adds []deltaAdd
	var removes []deltaRemove
	for seq, part := range parts {
		var records []Record
		for _, p := range byPart[part] {
			recs, err := o.readParquet(ctx, p)
			if err != nil {
				return res, fmt.Errorf("read %s: %w", p, err)
			}
			records = append(records, recs...)
		}
		parquetBytes, err := o.encodeParquet(records, schema)
		if err != nil {
			return res, fmt.Errorf("encode parquet: %w", err)
		}
		pKey := parquetKey(table, version, seq, part)
		if err := o.obj.Put(ctx, o.bucket, pKey, bytes.NewReader(parquetBytes), int64(len(parquetBytes)), "application/vnd.apache.parquet"); err != nil {
			return res, fmt.Errorf("put compacted parquet: %w", err)
		}
		adds = append(adds, deltaAdd{
			Path:             relTablePath(table, pKey),
			PartitionValues:  partitionValues(part),
			Size:             int64(len(parquetBytes)),
			ModificationTime: now.UnixMilli(),
			DataChange:       true,
			Stats:            deltaStats(records, schema.PartitionBy),
		})
		for _, p := range byPart[part] {
			removes = append(removes, deltaRemove{
				Path: relTablePath(table, p), DeletionTimestamp: now.UnixMilli(), DataChange: true,
			})
		}
		res.Rows += len(records)
	}
	commit, err := buildDeltaCommit(false, nil, adds, removes)
	if err != nil {
		return res, fmt.Errorf("build compact commit: %w", err)
	}
	if err := o.putLogFile(ctx, table, version, commit); err != nil {
		return res, err
	}

	res.FilesAfter = len(paths) - len(removes) + len(adds)
	o.log.Info("datalake compact", "table", table,
		"files_before", res.FilesBefore, "files_after", res.FilesAfter,
		"partitions_compacted", len(parts), "rows", res.Rows)
	return res, nil
}

// PurgeRows is the GDPR filtered rewrite: it drops every row matching match
// from the table's active file set in ONE atomic Delta commit, so a reader
// never sees a half-purged state. Each file CONTAINING matches is rewritten
// in place (same partition directory, minus the matching rows) and the old
// file removed; untouched files stay untouched — with daily partitioning a
// purge rewrites only the days the user actually appears in, preserving the
// layout and the immutability of every other partition. A table where
// nothing matches is a no-op with no commit. Returns how many rows were
// removed.
//
// Delta-log semantics note: the removed files' bytes stay in object storage
// (time travel could still read them). Vacuum is the storage-level companion
// that physically deletes them after the grace window.
func (o *ObjectStore) PurgeRows(ctx context.Context, table string, match func(Record) bool) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	txns, err := o.Log(ctx, table)
	if err != nil {
		return 0, err
	}
	paths, schema, haveSchema := activeFiles(txns)
	if len(paths) == 0 || !haveSchema {
		return 0, nil // empty (or never-written) table — nothing to purge
	}

	version, err := o.nextVersionLocked(ctx, table)
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	var adds []deltaAdd
	var removes []deltaRemove
	removed, kept, seq := 0, 0, 0
	for _, p := range paths {
		recs, err := o.readParquet(ctx, p)
		if err != nil {
			return 0, fmt.Errorf("read %s: %w", p, err)
		}
		var keep []Record
		dropped := 0
		for _, rec := range recs {
			if match(rec) {
				dropped++
			} else {
				keep = append(keep, rec)
			}
		}
		if dropped == 0 {
			continue // file untouched — stays active, no rewrite
		}
		removed += dropped
		kept += len(keep)
		removes = append(removes, deltaRemove{
			Path: relTablePath(table, p), DeletionTimestamp: now.UnixMilli(), DataChange: true,
		})
		if len(keep) == 0 {
			continue // every row matched — remove-only, no remainder file
		}
		parquetBytes, err := o.encodeParquet(keep, schema)
		if err != nil {
			return 0, fmt.Errorf("encode purged parquet: %w", err)
		}
		part, _ := partitionOfPath(p)
		pKey := parquetKey(table, version, seq, part)
		seq++
		if err := o.obj.Put(ctx, o.bucket, pKey, bytes.NewReader(parquetBytes), int64(len(parquetBytes)), "application/vnd.apache.parquet"); err != nil {
			return 0, fmt.Errorf("put purged parquet: %w", err)
		}
		adds = append(adds, deltaAdd{
			Path:             relTablePath(table, pKey),
			PartitionValues:  partitionValues(part),
			Size:             int64(len(parquetBytes)),
			ModificationTime: now.UnixMilli(),
			DataChange:       true,
			Stats:            deltaStats(keep, schema.PartitionBy),
		})
	}
	if removed == 0 {
		return 0, nil
	}
	commit, err := buildDeltaCommit(false, nil, adds, removes)
	if err != nil {
		return 0, fmt.Errorf("build purge commit: %w", err)
	}
	if err := o.putLogFile(ctx, table, version, commit); err != nil {
		return 0, err
	}
	o.log.Info("datalake purge", "table", table,
		"rows_removed", removed, "rows_kept_in_rewrites", kept,
		"files_rewritten", len(adds), "files_untouched", len(paths)-len(removes))
	return removed, nil
}

// EnsurePartitioned migrates a flat-layout table to the daily-partitioned
// layout schema.PartitionBy describes: every active file is read, rows are
// regrouped by event_date, and ONE atomic commit writes the new metaData
// (partitionColumns + partition-source configuration), adds the partitioned
// files, and removes every flat file — a reader sees the old layout or the
// new, never a mix. Re-encoding uses the CALLER's schema, so rows gain any
// columns added since they were written (as nulls). No-op when the schema is
// unpartitioned, the table doesn't exist yet (the first Write creates it
// partitioned), or the table is already partitioned. Old bytes are reclaimed
// by the next Vacuum. Called by the pipeline at boot for each sink table.
func (o *ObjectStore) EnsurePartitioned(ctx context.Context, table string, schema Schema) error {
	if schema.PartitionBy == "" {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()

	txns, err := o.Log(ctx, table)
	if err != nil {
		return err
	}
	if len(txns) == 0 {
		return nil // no table yet — Write will create it partitioned
	}
	paths, recovered, haveSchema := activeFiles(txns)
	if haveSchema && recovered.PartitionBy != "" {
		// Already partitioned — but sweep STRAY flat files: a rolled-back
		// old writer (pre-partitioning binary) appends root-level files a
		// partitioned table should no longer have. Rewrite just those into
		// their partitions; properly-partitioned files stay untouched.
		var strays []string
		for _, p := range paths {
			if _, ok := partitionOfPath(p); !ok {
				strays = append(strays, p)
			}
		}
		if len(strays) == 0 {
			return nil
		}
		paths = strays
	}

	var records []Record
	for _, p := range paths {
		recs, err := o.readParquet(ctx, p)
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		records = append(records, recs...)
	}

	version, err := o.nextVersionLocked(ctx, table)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	md, err := deltaMetaDataFor(deltaTableID(table), schema, now.UnixMilli())
	if err != nil {
		return fmt.Errorf("build delta metadata: %w", err)
	}
	var adds []deltaAdd
	if len(records) > 0 {
		keys, groups := groupByPartition(records, schema.PartitionBy)
		for seq, part := range keys {
			group := groups[part]
			parquetBytes, err := o.encodeParquet(group, schema)
			if err != nil {
				return fmt.Errorf("encode parquet: %w", err)
			}
			pKey := parquetKey(table, version, seq, part)
			if err := o.obj.Put(ctx, o.bucket, pKey, bytes.NewReader(parquetBytes), int64(len(parquetBytes)), "application/vnd.apache.parquet"); err != nil {
				return fmt.Errorf("put parquet: %w", err)
			}
			adds = append(adds, deltaAdd{
				Path:             relTablePath(table, pKey),
				PartitionValues:  partitionValues(part),
				Size:             int64(len(parquetBytes)),
				ModificationTime: now.UnixMilli(),
				DataChange:       true,
				Stats:            deltaStats(group, schema.PartitionBy),
			})
		}
	}
	removes := make([]deltaRemove, 0, len(paths))
	for _, p := range paths {
		removes = append(removes, deltaRemove{
			Path: relTablePath(table, p), DeletionTimestamp: now.UnixMilli(), DataChange: true,
		})
	}
	commit, err := buildDeltaCommit(false, &md, adds, removes)
	if err != nil {
		return fmt.Errorf("build repartition commit: %w", err)
	}
	if err := o.putLogFile(ctx, table, version, commit); err != nil {
		return err
	}
	o.log.Info("datalake repartitioned", "table", table,
		"partition_by", schema.PartitionBy, "rows", len(records),
		"files_before", len(paths), "files_after", len(adds))
	return nil
}

// VacuumResult reports what a Vacuum pass physically deleted.
type VacuumResult struct {
	Table        string
	FilesDeleted int
	BytesFreed   int64
}

// Vacuum PHYSICALLY deletes tombstoned Parquet files — the storage-level
// companion to PurgeRows and Compact. A Delta remove action only drops a
// file from the active set; its bytes stay in the bucket (time-travel
// semantics), which for a GDPR purge means the user's data still exists on
// disk even though every reader sees it gone. Vacuum deletes files whose
// remove action is older than grace; the grace window covers in-flight
// readers that replayed the log just before the tombstone (all our readers
// — Read, Compact, DuckDB delta_scan — resolve the active set from the log,
// so only a reader mid-flight at tombstone time can still want the bytes).
// Time travel to pre-vacuum versions is deliberately given up, matching
// Delta's own VACUUM contract.
//
// Crashed-write orphans (parquet PUT succeeded, log PUT didn't) are NOT
// vacuum's problem: max+1 version allocation means the next write reuses
// the same version number and overwrites the orphan before referencing it —
// self-healing. Only a table whose FINAL write crashed retains one orphan
// file, which is negligible.
func (o *ObjectStore) Vacuum(ctx context.Context, table string, grace time.Duration) (VacuumResult, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	res := VacuumResult{Table: table}
	txns, err := o.Log(ctx, table)
	if err != nil {
		return res, err
	}
	active := map[string]bool{}
	size := map[string]int64{}
	removedAt := map[string]time.Time{}
	for _, t := range txns {
		switch t.Action {
		case "add":
			active[t.Path] = true
			size[t.Path] = t.ByteSize
			delete(removedAt, t.Path) // re-add (never happens with our writer, but be safe)
		case "remove":
			delete(active, t.Path)
			if t.Timestamp.After(removedAt[t.Path]) {
				removedAt[t.Path] = t.Timestamp
			}
		}
	}
	cutoff := time.Now().UTC().Add(-grace)
	for path, ts := range removedAt {
		if active[path] || ts.After(cutoff) {
			continue
		}
		if err := o.obj.Delete(ctx, o.bucket, path); err != nil {
			return res, fmt.Errorf("vacuum delete %s: %w", path, err)
		}
		res.FilesDeleted++
		res.BytesFreed += size[path]
	}
	if res.FilesDeleted > 0 {
		o.log.Info("datalake vacuum", "table", table, "files_deleted", res.FilesDeleted, "bytes_freed", res.BytesFreed)
	}
	return res, nil
}

// CountRows counts rows matching match across the active file set — the
// read-only verification counterpart of PurgeRows (privacy-verify residual
// checks).
func (o *ObjectStore) CountRows(ctx context.Context, table string, match func(Record) bool) (int, error) {
	txns, err := o.Log(ctx, table)
	if err != nil {
		return 0, err
	}
	paths, _, _ := activeFiles(txns)
	n := 0
	for _, p := range paths {
		recs, err := o.readParquet(ctx, p)
		if err != nil {
			return 0, fmt.Errorf("read %s: %w", p, err)
		}
		for _, rec := range recs {
			if match(rec) {
				n++
			}
		}
	}
	return n, nil
}

// activeFiles replays a transaction list into the sorted active file set plus
// the latest recorded schema. Shared by Compact / PurgeRows / CountRows.
func activeFiles(txns []Transaction) (paths []string, schema Schema, haveSchema bool) {
	active := map[string]bool{}
	for _, t := range txns {
		if t.Schema != nil {
			schema = *t.Schema
			haveSchema = true
		}
		switch t.Action {
		case "add":
			active[t.Path] = true
		case "remove":
			delete(active, t.Path)
		}
	}
	paths = make([]string, 0, len(active))
	for p := range active {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths, schema, haveSchema
}

// nextVersionLocked allocates the next Delta version: HIGHEST existing
// version + 1, never a count. Counting diverges the moment anything is
// non-uniform — the original bug counted log TRANSACTIONS in Compact but
// log FILES in Write, so a later flush eventually overwrote a compaction
// commit, silently resurrecting its tombstoned files as active… whose bytes
// Vacuum had by then legitimately deleted. Max+1 is also immune to
// historical gaps that the old allocator left behind. Caller must hold mu.
func (o *ObjectStore) nextVersionLocked(ctx context.Context, table string) (int, error) {
	keys, err := o.obj.List(ctx, o.bucket, deltaLogPrefix(table))
	if err != nil {
		return 0, fmt.Errorf("list delta log: %w", err)
	}
	next := 0
	for _, k := range keys {
		if v, ok := versionFromLogKey(k); ok && v >= next {
			next = v + 1
		}
	}
	return next, nil
}

// putLogFile writes one Delta commit file at the given version. Overwriting
// an existing commit would silently drop its adds/removes from the log
// (corrupting the active set), so an existing key fails loudly — under the
// single-writer lock it can only mean a version-allocation bug.
func (o *ObjectStore) putLogFile(ctx context.Context, table string, version int, body []byte) error {
	key := deltaLogKey(table, version)
	if exists, err := o.obj.Exists(ctx, o.bucket, key); err == nil && exists {
		return fmt.Errorf("delta log v%d already exists for %s — version allocation bug, refusing to overwrite", version, table)
	}
	if err := o.obj.Put(ctx, o.bucket, key, bytes.NewReader(body), int64(len(body)), "application/json"); err != nil {
		return fmt.Errorf("put delta log v%d: %w", version, err)
	}
	return nil
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
	return ParquetRecords(ctx, raw)
}

// ParquetRecords decodes a raw Parquet file (any schema) into Records.
// Exported for callers outside the Delta log flow — e.g. the onboarding
// drop-zone ingesting provider-delivered Parquet audience files.
func ParquetRecords(ctx context.Context, raw []byte) ([]Record, error) {
	mem := memory.NewGoAllocator()
	tbl, err := pqarrow.ReadTable(ctx, bytes.NewReader(raw),
		parquet.NewReaderProperties(mem), pqarrow.ArrowReadProperties{}, mem)
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
