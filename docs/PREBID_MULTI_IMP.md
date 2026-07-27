# Prebid multi-impression support — deferred design note

**Status:** DEFERRED (deliberate). Documented after reading both hot paths end
to end, so whoever picks it up starts from the design, not "add a loop."

**Recommendation:** don't build this as a checkbox. Build it only as a focused
effort whose *first* deliverable is the per-imp `trace_id` model — because the
real work is a pipeline-wide change to the billing/tracing spine, not the two
auction handlers. If the motivation is "close the gap," skip it: today's
single-imp path is a scoping limitation, not a correctness bug (nothing
mis-bills; a multi-imp request simply fills the first slot). If the motivation
is prod-parity (real Prebid.js *always* sends one auction per page with all ad
units as separate imps), it's legitimate — go in knowing the scope below.

Blocks `tests/e2e/competitive_auction_test.go`
`TestCompetitiveG1_PrebidMultiImpRequestPerImpAuction` (skipped).

---

## What "multi-imp" means

One OpenRTB `BidRequest` carries `Imp[]` with N entries (N ad slots on a page
loaded together). Each imp should get its OWN auction; the response is one
`BidResponse` with up to N winning bids, each `BidObj.ImpID` pointing at its imp.

## Current behaviour (both hardcode `Imp[0]`)

- **Exchange** `cmd/exchange/main.go` `auctionHandler` (~500–778): reads
  `Imp[0].TagID` (placement), `Imp[0].BidFloor` (deal eval + auction floor),
  builds ONE response `SeatBid` with `ImpID: Imp[0].ID`, emits ONE
  `AuctionWinEvent`/`AuctionCompleteEvent`, ONE win/loss fan-out, ONE
  `router.RecordWin`.
- **DSP** `cmd/dsp/main.go` bid handler (~896–1141): reads `Imp[0]` for
  floor/format/dimensions, runs the campaign loop once, returns ONE `bestBid`
  keyed to `Imp[0].ID`. The competitor pods share this code.
- **`auction.Bid`** (`pkg/auction/strategy.go`) has NO `ImpID` field — so bids
  returned from the DSP fan-out can't even be grouped per imp without a
  core-type change.

## The real blocker — the money/trace invariant

`AuctionWinEvent` is **the single source of truth for cost** (see CLAUDE.md /
docs/PLAN.md), keyed on `trace_id`; the tracker's exactly-once dedup and the
billing reserve/settle both key on `trace_id`. If one request wins N imps, N ads
render and N impressions fire — but they'd all carry the request's single
`trace_id`, so:

- the tracker dedup collapses them to one impression, and
- billing settles one cost for N rendered ads → **silent under-billing** and a
  fill-rate over/under-count. This is exactly the "data slippage" the platform
  exists to avoid.

So each winning imp needs its **own** `trace_id` (e.g. `traceID + ":" + imp.ID`,
or a fresh id per win), threaded through render → track → bill so each impression
is independently billable and traceable. That makes this a **pipeline-wide**
change, not a two-handler change.

## Files that must change (the honest scope)

1. **`pkg/auction/strategy.go`** — add `ImpID string` to `auction.Bid`.
2. **`cmd/dsp/main.go`** — loop the format-extraction + campaign-eval over
   `bidReq.Imp`, collect a bid per imp, return multiple `BidObj` (grouped into
   `SeatBid`s by seat), each `ImpID = imp.ID`. Behaviour must be identical for a
   single-imp request.
3. **`cmd/exchange/main.go`** — `fanOutToDSPs` must populate `auction.Bid.ImpID`
   from each DSP `BidObj.ImpID`; then loop `auctionHandler` over `bidReq.Imp`:
   per imp filter bids by `ImpID`, do deal eval (per-imp placement + floor),
   run the auction, build a per-imp `SeatBid` (assemble all into one response),
   and emit per-imp win/complete events + win/loss + `RecordWin` under a
   per-imp `trace_id`.
4. **The trace-id-per-imp model** — the load-bearing decision. Define how a
   per-imp id is derived and PROPAGATED so it survives:
   - SSP render / `pkg/grpcx` response (winner carries its imp's id),
   - ad server + tracker (impression/click/view fire the per-imp id),
   - billing (`AuctionWinEvent` + reserve/settle keyed on the per-imp id),
   - the trace explorer (portal timeline groups children under the parent).
   This is where the design effort lives.
5. **Downstream that reads `trace_id`** — audit tracker dedup (Nats-Msg-Id +
   biz-key), billing `SettleByTrace`/reservation lookup, reporting
   attribution, and the trace inspector, so each treats a per-imp id correctly.

## Test + regression gate

- Un-skip `TestCompetitiveG1_PrebidMultiImpRequestPerImpAuction`: send a 2-imp
  request (one matchable placement, one non-existent), assert one `SeatBid` for
  the valid imp and no bid for the other.
- Add a POSITIVE case: 2 valid imps → 2 wins → 2 INDEPENDENT impressions bill 2
  separate costs (the invariant this whole thing is about).
- **Regression:** re-run the FULL single-imp competitive + billing + pacing
  suites — every existing single-imp assertion must be byte-for-byte unchanged.

## Why the incremental "just loop the handler" tempts and fails

You can make the exchange loop over imps and return N seatbids without the
trace-id work, and `TestCompetitiveG1` (which only checks the response shape)
would pass. But the moment two imps WIN and render, billing under-counts. A
green G1 with a broken money invariant is worse than an honest skip — so the
trace-id model is a hard prerequisite, not a follow-up.
