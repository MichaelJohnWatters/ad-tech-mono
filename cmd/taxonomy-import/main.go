// cmd/taxonomy-import loads the OFFICIAL IAB Tech Lab Audience Taxonomy file
// into iab_audience_taxonomy, replacing the demo subset seeded by migration
// 062 with the certified node ids external parties expect behind segtax=4.
//
// Host-runnable one-off (invoice-runner pattern):
//
//	go run ./cmd/taxonomy-import --file "Audience Taxonomy 1.1.tsv"
//
// The official file is distributed by IAB Tech Lab (free licence,
// iabtechlab.com) as a spreadsheet — export the taxonomy sheet as TSV/CSV
// first. The importer is column-order agnostic: it locates the header row
// and reads "Unique ID", "Parent ID", and "Name" case-insensitively, then
// derives each node's breadcrumb path by walking the parent chain (so tier
// columns are not required).
//
// Idempotent: rows upsert by id, in two passes (insert with NULL parent,
// then set parents) so file ordering never violates the parent FK. --prune
// deletes rows absent from the file — except nodes still referenced by a
// segment's taxonomy_id, which are kept with a warning (relabel those
// segments, then re-run with --prune).
package main

import (
	"bufio"
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	_ "github.com/lib/pq"
)

type node struct {
	id     int64
	parent int64 // 0 = root
	name   string
}

func main() {
	file := flag.String("file", "", "path to the official Audience Taxonomy export (TSV, or CSV with --sep=,)")
	sep := flag.String("sep", "\t", "column separator")
	prune := flag.Bool("prune", false, "delete taxonomy rows absent from the file (referenced nodes are kept with a warning)")
	flag.Parse()

	log := logger.New("taxonomy-import")
	if *file == "" {
		log.Error("--file is required (export the official IAB Audience Taxonomy sheet as TSV)")
		os.Exit(1)
	}

	nodes, err := parseFile(*file, *sep)
	if err != nil {
		log.Error("parse taxonomy file", "error", err)
		os.Exit(1)
	}
	if len(nodes) == 0 {
		log.Error("no taxonomy rows found — is the header row present (Unique ID / Parent ID / Name)?")
		os.Exit(1)
	}

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = routes.DefaultPostgresURL
	}
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Error("open db", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	inserted, pruned, kept, err := importNodes(ctx, db, nodes, *prune)
	if err != nil {
		log.Error("import failed (rolled back)", "error", err)
		os.Exit(1)
	}
	log.Info("taxonomy imported", "nodes", inserted, "pruned", pruned, "kept_referenced", kept)
}

// parseFile reads the export and returns the taxonomy nodes. The header row
// is located by its "unique id" column; everything before it is ignored
// (official sheets carry licence banners above the header).
func parseFile(path, sep string) ([]node, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)

	idCol, parentCol, nameCol := -1, -1, -1
	var out []node
	for sc.Scan() {
		cols := strings.Split(sc.Text(), sep)
		if idCol == -1 {
			// Still hunting for the header row.
			for i, c := range cols {
				switch strings.ToLower(strings.TrimSpace(c)) {
				case "unique id", "uniqueid", "id":
					idCol = i
				case "parent id", "parentid", "parent":
					parentCol = i
				case "name", "condensed name (1st, 2nd, last tier)", "condensed name":
					if nameCol == -1 { // prefer the plain Name column
						nameCol = i
					}
				}
			}
			if idCol == -1 || nameCol == -1 {
				idCol = -1 // not the header — keep hunting
			}
			continue
		}
		get := func(i int) string {
			if i < 0 || i >= len(cols) {
				return ""
			}
			return strings.TrimSpace(cols[i])
		}
		id, err := strconv.ParseInt(get(idCol), 10, 64)
		if err != nil || id <= 0 {
			continue // blank/annotation row
		}
		n := node{id: id, name: get(nameCol)}
		if p, err := strconv.ParseInt(get(parentCol), 10, 64); err == nil && p > 0 {
			n.parent = p
		}
		if n.name == "" {
			continue
		}
		out = append(out, n)
	}
	return out, sc.Err()
}

// importNodes writes the nodes in one transaction: upsert ids first (NULL
// parent, so file order can't violate the FK), then set parents, then
// recompute every breadcrumb path by walking the parent chain, then
// optionally prune.
func importNodes(ctx context.Context, db *sql.DB, nodes []node, prune bool) (imported, pruned, kept int, err error) {
	byID := make(map[int64]node, len(nodes))
	for _, n := range nodes {
		byID[n.id] = n
	}
	var pathOf func(id int64, depth int) string
	pathOf = func(id int64, depth int) string {
		n, ok := byID[id]
		if !ok || depth > 20 { // depth guard: a parent cycle in the file must not hang the import
			return ""
		}
		if n.parent == 0 {
			return n.name
		}
		if pp := pathOf(n.parent, depth+1); pp != "" {
			return pp + " | " + n.name
		}
		return n.name
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, 0, err
	}
	defer tx.Rollback()

	for _, n := range nodes {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO iab_audience_taxonomy (id, parent_id, name, path)
VALUES ($1, NULL, $2, $3)
ON CONFLICT (id) DO UPDATE SET parent_id = NULL, name = EXCLUDED.name, path = EXCLUDED.path`,
			n.id, n.name, pathOf(n.id, 0)); err != nil {
			return 0, 0, 0, fmt.Errorf("upsert node %d: %w", n.id, err)
		}
	}
	for _, n := range nodes {
		if n.parent == 0 {
			continue
		}
		if _, ok := byID[n.parent]; !ok {
			continue // parent not in file — leave as root rather than break the FK
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE iab_audience_taxonomy SET parent_id = $2 WHERE id = $1`, n.id, n.parent); err != nil {
			return 0, 0, 0, fmt.Errorf("set parent of %d: %w", n.id, err)
		}
	}

	if prune {
		ids := make([]int64, 0, len(nodes))
		for id := range byID {
			ids = append(ids, id)
		}
		// Keep nodes a segment still references — deleting them would break
		// the taxonomy_id FK and silently unlabel live segments.
		if err := tx.QueryRowContext(ctx, `
SELECT count(*) FROM iab_audience_taxonomy t
WHERE NOT (t.id = ANY($1::bigint[]))
  AND EXISTS (SELECT 1 FROM audience_segments s WHERE s.taxonomy_id = t.id)`,
			int64Array(ids)).Scan(&kept); err != nil {
			return 0, 0, 0, fmt.Errorf("count referenced strays: %w", err)
		}
		res, err := tx.ExecContext(ctx, `
DELETE FROM iab_audience_taxonomy t
WHERE NOT (t.id = ANY($1::bigint[]))
  AND NOT EXISTS (SELECT 1 FROM audience_segments s WHERE s.taxonomy_id = t.id)
  AND NOT EXISTS (SELECT 1 FROM iab_audience_taxonomy c WHERE c.parent_id = t.id AND c.id = ANY($1::bigint[]))`,
			int64Array(ids))
		if err != nil {
			return 0, 0, 0, fmt.Errorf("prune: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			pruned = int(n)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, 0, err
	}
	return len(nodes), pruned, kept, nil
}

// int64Array renders a pq-compatible bigint array literal without pulling in
// pq.Array's driver-value plumbing for a one-off tool.
func int64Array(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return "{" + strings.Join(parts, ",") + "}"
}
