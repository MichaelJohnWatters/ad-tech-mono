# taxonomy-import Tool

Loads the OFFICIAL IAB Tech Lab Audience Taxonomy export into
`iab_audience_taxonomy`, replacing the demo subset seeded by migration 062 with
the certified node ids external parties expect behind `user.data ext.segtax=4`.
The demo ids are platform-local stand-ins — fine for local dev/portal/e2e, NOT
certified for cross-party interop; this importer is how you get real interop.

## How it runs

- Host-runnable one-off only (invoice-runner pattern, never deployed — no helm entry):
  `go run ./cmd/taxonomy-import --file "Audience Taxonomy 1.1.tsv" [--sep=,] [--prune]`
- The official file is a spreadsheet (free licence, iabtechlab.com) — export the
  taxonomy sheet as TSV/CSV first. The parser is column-order agnostic: it hunts
  for the header row ("Unique ID" / "Parent ID" / "Name", case-insensitive,
  licence banners above are skipped) and derives each node's breadcrumb `path`
  by walking the parent chain — tier columns not required.
- Only env: `DATABASE_URL` (defaults to `routes.DefaultPostgresURL`).

## Idempotency & gotchas

- Idempotent: one transaction, upsert by id in two passes — insert all with
  NULL parent, then set parents — so file ordering can never violate the
  self-referencing FK. Parents absent from the file are left as roots.
- `--prune` deletes rows absent from the file, EXCEPT (a) nodes still referenced
  by a segment's `taxonomy_id` (kept + counted as `kept_referenced` — deleting
  would silently unlabel live segments; relabel, then re-run with --prune) and
  (b) nodes that are the parent of an in-file node (keeps the parent FK intact).
- A parent cycle in the file can't hang the import (depth-20 guard on path walk).
- `iab_audience_taxonomy` is global reference data: no `account_id`, no RLS
  (config_schema precedent) — no tenant GUC/platform-hatch needed here.

## Pointers

- `docs/PLAN.md` -> "Unified Audience Management Layer" -> "Data Taxonomy"
- Table + demo seed rationale: `migrations/062_iab_audience_taxonomy.sql` (header comment)
