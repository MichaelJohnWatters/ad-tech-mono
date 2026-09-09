# Migrate Job

Goose migration runner. Applies `migrations/*.sql` to Postgres; runs on every deploy.

## How it runs

- **In-cluster (normal path):** helm **post**-install/post-upgrade hook Job
  (`k8s/helm/adtech/templates/migrate-job.yaml`) on every `helm upgrade` /
  `make stack-up`. Post- not pre-: on a fresh cluster postgres doesn't exist yet;
  an init container gates on postgres, app pods crashloop-then-recover meanwhile.
  The Job self-deletes on success — `make stack-up` waits on `goose_db_version`
  existing, not on the Job.
- **Host:** `make migrate` / `make migrate-status` (`go run ./cmd/migrate [up|down|status|reset]`;
  arg passes straight to goose, default `up`).

## Gotchas

- Idempotent: goose tracks applied versions in `goose_db_version`; re-runs are no-ops.
- Env: `DATABASE_URL` (default `routes.DefaultPostgresURL`) — must be the **owner**
  role (`adtech`), never the RLS-flipped `adtech_app`. `MIGRATIONS_DIR` (default `migrations`).
- Migrations are read from disk at runtime, NOT embedded — hence the dedicated
  `build/Dockerfile.migrate` (binary + `COPY migrations /migrations`); the generic
  Dockerfile ships only the binary and won't work here.
- No config/lifecycle/health packages — plain fatal-on-error binary that exits.

SQL conventions (numbering, Up/Down, RLS-per-table): `migrations/CLAUDE.md`.
