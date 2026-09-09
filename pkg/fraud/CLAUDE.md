# pkg/fraud - Fraud Detection & Seller Authorisation

Pure library for invalid-traffic defence: hot-path real-time checks (tracker),
weighted batch scoring (model only — no live caller yet), and ads.txt /
app-ads.txt / sellers.json seller authorisation (exchange gate + crawler jobs).
No DB/NATS/Redis imports — callers own persistence and caching.

## Key Entry Points

- `realtime.go` — `NewRealTimeChecker(Config)` → `Check(Request) CheckResult`.
  In-memory, <1ms budget: hardcoded bot-UA patterns, datacenter CIDRs, per-IP
  rate window, plus DB-sourced blocklists pushed in via
  `ReplaceBlocklists([]BlocklistEntry)` (mirrors the `fraud_blocklists` table).
  `Blocked` when score ≥ `Config.BlockThreshold` (default 0.7).
- `scoring.go` — `NewScorer(SignalWeights)` → `Score(map[string]bool) ScoreResult`;
  weight-normalised composite score, categorised `sivt`/`givt`/`clean` per IAB IVT.
- `adstxt.go` — `FetchAdsTxt` (GET `https://{domain}/ads.txt`, 5 MiB cap, status
  `valid|missing|error`), `ParseAdsTxt`, `AdsTxtCache.IsAuthorised(pubDomain,
  ourDomain, ourAccountID)` → `authorised|not_listed|no_ads_txt`;
  `GenerateSellersJSON` builds the platform sellers.json.
- `appads.go` — `FetchAppAdsTxt` (app developer domain, same line format —
  reuses `AdsTxtEntry`/`ParseAdsTxt`; mirrors `app_ads_txt_cache`).

## Invariants & Gotchas

- **Hot-path rule:** `Check` must stay allocation-light and network-free — it
  runs inline on every tracker request (`tracker.fraud_enabled`, default true).
  Never add I/O here; DB-sourced rules arrive via the tracker's warm cache
  (`cache.warm.fraud_rules.poll_interval` + `adtech.cache.invalidate.fraud-rules`).
- `ReplaceBlocklists` swaps ONLY the DB-sourced sets (`type=ip|ua`); hardcoded
  bot patterns, datacenter ranges, and manual `BlockIP` entries survive a refresh.
  A refresh must never clobber them — keep that separation.
- Rate limiting is **per-pod** (in-process map, 1-minute window,
  `MaxRequestsPerIPPerMinute` default 60). More tracker replicas = proportionally
  looser effective limit; don't treat it as a cluster-wide cap.
- The client IP fed to `Check` must be the trusted-hop-resolved one
  (`tracker.trusted_proxy_hops` / `pkg/clientip`) — a forged XFF prefix would
  otherwise dodge the IP blocklist.
- ads.txt `missing` (404) is a legitimate state, not an error — don't "fix" it.
  Enforcement mode lives in the exchange (`exchange.adstxt_enforcement`
  off|warn|strict + `adstxt_seller_domain`/`_id`), not here.
- `Scorer` currently has **no `cmd/` caller** — it's the model the unbuilt batch
  fraud CronJob would wrap (see `cmd/fraud/CLAUDE.md` for why it's blocked and
  the build sequence). Don't claim a batch sweep runs.
- `fraud_blocklists` is platform-global (no `account_id`, migration 016) —
  one of the few justified non-tenant tables.

## Used By

- `cmd/tracker` — `RealTimeChecker` inline on `/v1/t/*` handlers + the SSAI
  media gate (`mediagate.go`); warm blocklist cache in `blocklist.go` (fails
  open to hardcoded patterns when `database.url` is unset).
- `cmd/exchange` — `AdsTxtCache` pre-fan-out gate, warm-cached from
  `ads_txt_cache` (`cache.warm.ads_txt.poll_interval`, invalidate subject
  `adtech.cache.invalidate.ads-txt`).
- `cmd/adstxt` / `cmd/appadstxt` — one-shot crawlers upserting fetch results
  (intended daily; NOT currently a Helm CronJob — see `cmd/adstxt/CLAUDE.md`).
- `cmd/gateway` — `GenerateSellersJSON` behind `routes.SellersJSON`; staff
  blocklist CRUD at `routes.APIFraudBlocklists` (JWT `fraud:*`, publishes the
  fraud-rules invalidate on change).

## Testing

Pure functions — plain unit tests (`fraud_test.go`, `realtime_blocklist_test.go`;
`adstxt_fetch_test.go` uses `httptest`, no mocks needed). E2E helpers in
`tests/e2e/harness/fraud.go`, live checks in `tests/e2e/fraud_test.go`.

## PLAN.md Pointers

- `docs/PLAN.md` -> "Fraud Detection and Traffic Quality" (Real-Time Detection
  (Hot Path), Batch Detection (Cold Path), Fraud Scoring)
- `docs/PLAN.md` -> "ads.txt and sellers.json" (ads.txt Verification, ads.txt
  Crawler, sellers.json)
