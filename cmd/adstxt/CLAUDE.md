# adstxt Job

One-shot ads.txt crawler. Fetches `https://{domain}/ads.txt` for every publisher
domain and upserts the parsed result into `ads_txt_cache` — the table the
exchange warm-caches to enforce seller authorisation
(`exchange.adstxt_enforcement`: off/warn/strict, plus
`exchange.adstxt_seller_domain`/`_seller_id`) before auction fan-out.

## How it runs

- One-shot: list domains (`SELECT DISTINCT domain FROM publishers WHERE domain <> ''`) → fetch →
  upsert → exit. 10-minute overall deadline; 10s HTTP timeout + 5 MiB body cap
  per domain (`pkg/fraud.FetchAdsTxt`).
- Intended cadence: daily CronJob (Kubernetes schedules it). **Not currently
  wired into the helm chart** — no `cronjobs:` entry in
  `k8s/helm/adtech/values.yaml` (only batch-conductor / dayboundary /
  invoice-runner / account-closeout are); run it as a one-off (`go run ./cmd/adstxt`).
- Config via `config.Setup("adstxt", nil, log)` — no service schema; reads only
  `database.url` and `nats.url`.

## Idempotency + change detection

- Upsert keyed on `domain` (PK); re-runs are safe.
- `last_changed` advances only when parsed entries actually differ
  (`IS DISTINCT FROM` in the upsert); a plain re-crawl bumps `last_fetched` only.
- If ≥1 domain changed, publishes `events.SubjectCacheInvalidateAdsTxt`
  (`adtech.cache.invalidate.ads-txt`) so the exchange re-reads immediately.
  Best-effort by design: NATS down → warn and exit 0; the exchange's
  `cache.warm.ads_txt.poll_interval` (300s) poll picks it up.

## Gotchas

- `ads_txt_cache` is platform-global — no `account_id`, no RLS (migration 016).
- Status semantics matter downstream: `missing` (404) is a legitimate state, not
  an error. The exchange loads only `valid` rows; missing/error domains stay
  absent so enforcement treats them as unverifiable (`no_ads_txt` → allowed),
  not `not_listed` (rejected). See `cmd/exchange/adstxt.go`.
- app-ads.txt is a **separate sibling job** (`cmd/appadstxt` →
  `app_ads_txt_cache`, migration 035); this binary handles web ads.txt only.

## Pointers

- `docs/PLAN.md` -> "ads.txt and sellers.json" (verification flow, crawler, enforcement)
- `docs/PLAN.md` -> "Fraud Detection and Traffic Quality"
- E2E: `tests/e2e/fraud_test.go` `TestFraudAdsTxtUnverifiedRejected` (enforcement knobs + rejection)
