# migrations/ - Database Migrations

Plain SQL files managed by goose. Embedded into `cmd/migrate/` binary via Go `embed.FS`.

## Conventions

- Files are numbered sequentially: `001_`, `002_`, etc.
- Each file has `-- +goose Up` and `-- +goose Down` sections
- One change per migration file
- Additive only in production - never rename/drop columns in the same release as code changes
- Every new table needs an RLS policy (added in migration 029 or a new migration)
- Every tenant-scoped table must have an `account_id` column

## Running Migrations

- **Locally:** Tilt runs the migrate job automatically
- **CI/CD:** Runs before deploying services. A pre-migration pg_dump backup is taken first.
- **Manual:** `go run ./cmd/migrate` or `goose up`

## Full migration list

See `docs/PLAN.md` -> "Migration Files" for the complete list of migrations.

## Adding a New Migration

1. Create `migrations/NNN_description.sql` with Up and Down sections
2. If the table is tenant-scoped, add `account_id UUID NOT NULL` column
3. Add an RLS policy for the new table (or add to migration 029)
4. Update `docs/PLAN.md` migration list
5. Tilt auto-detects the new file and reruns migrations
