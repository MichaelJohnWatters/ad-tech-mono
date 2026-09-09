# appadstxt Job

app-ads.txt crawler — the in-app twin of `cmd/adstxt` (IAB app-ads.txt is served from the app
*developer's* website domain, not the app bundle id). One-shot: fetch all, upsert, exit.

## What it does

- Lists `SELECT DISTINCT developer_domain FROM publishers WHERE developer_domain <> ''`
  (web-only publishers have `''` and are skipped — migration 035 added the column).
- `fraud.FetchAppAdsTxt` (`pkg/fraud/appads.go`) GETs `https://{developer_domain}/app-ads.txt`,
  parses via the shared `ParseAdsTxt` (line format identical to ads.txt), 5 MiB body cap,
  status `valid | missing | error` — 404 is `missing`, a legitimate state, not an error.
- Upserts into `app_ads_txt_cache` keyed by developer domain. Change detection matches
  `cmd/adstxt`: `last_changed` advances only on insert or content change, and
  `RETURNING (last_changed = last_fetched)` reports changed-this-run. Idempotent; re-runs safe.

## How it runs

- Designed as a daily CronJob, Kubernetes owns the cadence; 10-minute overall context, 10s
  per-fetch HTTP timeout. **Not yet in the Helm `cronjobs:` block** (`k8s/helm/adtech/values.yaml`
  currently lists only batch-conductor / dayboundary / invoice-runner / account-closeout) — run
  manually with `go run ./cmd/appadstxt` for now.
- Config: `config.Setup("appadstxt", nil, log)`; only key read is `keys.Database.URL`.

## Gotchas

- **No NATS publish, unlike `cmd/adstxt`** — nothing warm-caches or enforces
  `app_ads_txt_cache` yet (the exchange only consumes `ads_txt_cache`). This job just keeps the
  table current; when enforcement lands, add the cache-invalidate publish + a Helm CronJob entry,
  and update the NATS Subjects table + event-flow diagram (see `docs/diagrams/README.md`).
- `app_ads_txt_cache` is platform-global fraud data with **intentionally no RLS** (migration 035,
  mirrors `ads_txt_cache` from 016) — the plain `database/sql` pool here is fine; don't copy this
  pattern for tenant-scoped tables.

## Pointers

- `docs/PLAN.md` -> "ads.txt and sellers.json" (crawler design, cache table shape, enforcement flow)
- `docs/PLAN.md` -> "Fraud Detection and Traffic Quality"
