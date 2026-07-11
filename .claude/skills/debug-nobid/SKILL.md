---
name: debug-nobid
description: Diagnose why an advertiser/DSP/format isn't winning auctions — empty portal, thin fill, a format that never serves, or "no bid". Works top-down from the exchange's fan-out routing decision down to the per-campaign eligibility gate. Use when a portal looks empty, a format won't fill, or fill rate is unexpectedly low.
---

# Diagnose a no-bid / thin fill

No-bid has layers. Debug **top-down** — a request must (1) reach the right DSP,
then (2) survive that DSP's per-campaign eligibility gate. Most "empty portal"
issues are one of: router excluded the DSP, missing format creative, budget/
balance exhausted, or targeting mismatch.

## Inputs

- **Namespace:** `adtech`. Ports: gateway 8080, exchange 8081, dsp-internal 8082,
  competitor DSPs 8089/8090, ssp 8084, publisher-adserver 8088.
- **Exchange smart-router debug:** `curl -s 'http://localhost:8081/debug/exchange/routing'`
  → per-`(channel,dsp)` stats. `?reset=true` clears them. `?preview=true&channel=<c>`
  shows who'd be selected right now.
- **DSP debug logging** (per-campaign reasons are logged at DEBUG):
  `kubectl -n adtech set env deploy/dsp-internal LOG_LEVEL=debug` → `kubectl -n adtech rollout status deploy/dsp-internal` → refresh its cache `curl -sX POST localhost:8082/debug/cache/refresh`. **Revert after:** `kubectl -n adtech set env deploy/dsp-internal LOG_LEVEL-`.
- **Postgres:** `kubectl -n adtech exec postgres-0 -- psql -U adtech -d adtech -tA -c "<SQL>"`.
- **Fire one format:** `go run ./cmd/simulator run --channel <native|audio|video|display> --requests 5 --rps 5`
  or hit a service directly, e.g. `curl -s "http://localhost:8084/v1/ssp/serve?placement_id=pl-sim-native&geo=USA&device=mobile&channel=native&user_id=u1"` (SSP returns `nobid:true` or a winner).
- **Reset budget counters** (rules out a stale/exhausted counter): `redis-cli FLUSHDB`.

## Procedure (top-down)

1. **Did the request reach a DSP that can fill it?** Read the exchange log:
   `kubectl -n adtech logs deploy/exchange --tail=20 | grep "auction started"` →
   `num_dsps_total` vs `num_dsps_called`. If `called < total`, the **smart router**
   dropped DSPs. Check `/debug/exchange/routing`: a DSP with `TotalCalls>20 &&
   BidRate<0.05` is **excluded** for that channel. Routing keys on the request's
   *format* — a DSP only holding native demand gets starved if it's judged on a
   blended rate. Fix by resetting stats (`?reset=true`) and confirm it recovers.
   *(This is exactly how native/audio demand got silently starved once.)*
2. **Reached the DSP but no bid?** Enable DSP debug logging (see Inputs), fire the
   request, and read the reason:
   `kubectl -n adtech logs deploy/dsp-internal --tail=60 | grep -iE "no matching creative|excluded by targeting|below floor|budget exhausted|balance_depleted|no eligible"`.
   The count `candidates:N` = campaigns evaluated. Each skipped one logs *why*.
3. **Map the reason to the gate** (map campaign UUID → name via psql):
   | Log line | Gate | Check |
   |---|---|---|
   | `no matching creative want_format:X` | creative | campaign needs a creative of format X; native needs `native_assets.title`, audio/video need `asset_url`+`duration_seconds` |
   | `excluded by targeting` | targeting | `targeting_rules.include_geo/device/...` vs the request's geo/device |
   | `campaign daily budget exhausted` | budget | `line_items.daily_budget` vs Redis spend counter — `FLUSHDB` to reset |
   | `balance_depleted` | money loop | `advertiser_balances.balance` for the account (0/no-row = fail-closed) |
   | `below floor` | price | campaign `base_bid` (× modifiers, ± competitor noise) vs `placements.floor_price` |
4. **Revert** `LOG_LEVEL` on the DSP.

## Format-specific creative facts (common trap)

The DSP's `selectCreativeForRequest` matches by the request's imp format:
- **native** → needs a creative with `format=native` AND `native_assets.title != ''`.
- **audio/video** → needs `format` match AND `asset_url` set AND `duration` within the
  request's min/max duration window.
- One line item can carry creatives of **several** formats — that's how a "display"
  campaign also bids video. To add a format to an advertiser, attach that creative.

## Quick psql lookups

```sql
-- campaigns by format + attached-creative summary
SELECT li.format, li.name, li.status, li.daily_budget,
  (SELECT string_agg(c.format,',') FROM line_item_creatives lic JOIN creatives c ON c.id=lic.creative_id WHERE lic.line_item_id=li.id)
FROM line_items li ORDER BY li.format;
-- which DSP an advertiser is on (internal vs competitor)
SELECT a.name, d.name, d.profile_type FROM accounts a LEFT JOIN dsps d ON d.id=a.dsp_id WHERE a.type='advertiser';
```
