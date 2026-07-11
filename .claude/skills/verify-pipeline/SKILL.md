---
name: verify-pipeline
description: Confirm ad events flow losslessly and accurately from auction → win → serve → tracker → NATS → reporting → ClickHouse, and that the reporting API matches raw analytics. Use when asked "is it working for real / with accurate numbers", after changing the auction/serving/tracking/reporting path, or to sanity-check the stack after a reset.
---

# Verify the ad pipeline end-to-end (accuracy)

The technique is **delta triangulation**: fire a *known* batch of traffic, then
check the same quantity at several independent stages. When independent sources
agree, the path is real and lossless. Reason in **deltas** (before/after a known
batch), not absolutes — the analytics tables accumulate across runs.

## Inputs

- **Stack must be up** (if unsure, run the `local-stack` skill first).
- **Namespace:** `adtech`. **ClickHouse:** `kubectl -n adtech exec clickhouse-0 -- clickhouse-client -q "<SQL>"`
- **Traffic generator:** `go run ./cmd/simulator run --profile <p> --requests <N> --rps 50`
  - `--profile trickle` = **display only** → cleanest accounting (1 format, 1:1 math). Use this for the accuracy math.
  - `--profile steady` = display/native/video/audio blend. Use to confirm all formats flow.
  - `--channel display|video|native|audio` forces one format.
  - Output line to capture: `Wins: <W> (<rate>%)  No-fill: <NF>  Errors: <E>`.
- **Clean counters (optional, for a controlled run):**
  `kubectl -n adtech exec deploy/redis -- redis-cli FLUSHDB` (budget counters) and
  `curl -s 'http://localhost:8081/debug/exchange/routing?reset=true'` (router stats).
- **Reporting API (portal path):** login `POST http://localhost:8080/v1/auth/login`
  (form fields `email`, `password=admin`), then `POST /v1/api/reports`
  `{"table":"impressions","metrics":["count","sum_cost"],"time_from":"<ISO>"}`.
- **Async settle:** the tracker→NATS→reporting→ClickHouse hop is async — **sleep ~8–10s**
  after firing before reading ClickHouse.

## Procedure

1. **Baseline snapshot** (before firing):
   ```
   CH="kubectl -n adtech exec clickhouse-0 -- clickhouse-client -q"
   $CH "SELECT count() FROM adtech.auctions"      # B_AUC
   $CH "SELECT count() FROM adtech.impressions"   # B_IMP
   $CH "SELECT count() FROM adtech.clicks"        # B_CLK
   ```
2. **Fire a known batch** and record `Wins`/`No-fill`/`Errors`:
   `go run ./cmd/simulator run --profile trickle --requests 200 --rps 50`
3. **Sleep 10s**, then snapshot again → compute deltas (`A_* - B_*`).
4. **Assert the invariants:**
   | Check | Expected | If it fails |
   |---|---|---|
   | `auctions` delta | **== N** (every request logs an auction, won *and* no-bid) | no-bid auctions being dropped → exchange only publishes AuctionComplete on win |
   | `impressions` delta | **== Wins** | slippage in serve→tracker→NATS→reporting |
   | fill = impressions/auctions | **== the simulator's win rate** | denominator wrong (see auctions row) |
   | `Errors` | **0** | a serving path is broken (service down, bad status handling) |
   | clicks delta | ~`ClickRate × Wins` (trickle ≈ 5%) | click beacon / tracker click path |
5. **Reporting API == raw ClickHouse** (proves the portal layer is lossless):
   - Pick a publisher (e.g. Publisher Simulator `5c4fbc65-…`), get its owner login via psql,
     log in, and query `/v1/api/reports` for `impressions` count+sum_cost over the day.
   - Compare to `SELECT count(), round(sum(clearing_price_usd),2) FROM adtech.impressions WHERE publisher_id='…' AND timestamp >= '<day> 00:00:00'`.
   - **Must match** (spend to sub-cent rounding).

## Interpreting a clean pass

"200 sent → auctions +200, impressions +160, fill = 160/200 = 80% = the reported
80% win rate, 0 errors, and the reporting API returns the same 160 the raw table
holds" ⇒ the pipeline is real and accurate. Report the numbers, not just "it works".

## Gotchas

- **Historical contamination:** if a fix changed what gets *written*, old rows are
  frozen wrong — a wide-window query mixes eras. Do a `make reset` (truncates
  ClickHouse) for a fully-consistent dataset, or query a window *after* the fix.
- **`trickle` is display-only.** To exercise native/audio/video use `steady` or `--channel`.
- Wrong-account confusion: traffic attributes to whatever account bids/wins — confirm
  which `publisher_id`/`account_id` you fired at before concluding "no data".
