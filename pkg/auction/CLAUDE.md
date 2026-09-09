# pkg/auction - Unified Auction Engine

Pure winner-selection logic with pluggable strategies. The Exchange hands it
already-fanned-out, deal-filtered bids; it picks winners and clearing prices.
No I/O, no store access, no NATS — everything upstream (fan-out, SmartRouter
in `pkg/optimise`, deal matching) and downstream (win events, billing) lives
in `cmd/exchange`.

## Key Entry Points

- `NewEngine(clk clock.Clock)` + `Engine.RunAuction(ctx, bids, request)` (`strategy.go`) —
  the pipeline: `SelectStrategy` (by `AuctionRequest.Channel`/`Format`) → common
  floor filter → strategy `Select` → `Result{Winners, LossBids, ShortFill}`.
- `Bid`, `AuctionRequest`, `PodRequest`, `Result`, `Winner`, `LossBid` (`strategy.go`) —
  the whole API surface. OpenRTB loss-reason constants (`LossBelowFloor`,
  `LossOutbid`, …) also live here.
- Strategies (all registered in `NewEngine`):
  - `single_winner` (`single.go`) — display/native/single video/audio/ingame-default;
    also the default for unknown channels. Implements both price modes.
  - `pod` (`pods.go`) — video/audio ad breaks (`Format == "pod"`); variable-duration
    (greedy by CPM/sec) and fixed-slot (`RqdDurs`) fills, with adjacency /
    unique-advertiser / unique-category separation and `MinCPMPerSec` floor.
    `ShortFill` = unfilled seconds/slots. `Pod == nil` falls back to single-winner.
  - `relevance_weighted` (`relevance.go`) — retail sponsored products; ranks by
    relevance × bid, `MinRelevance` eligibility floor, fills `SlotCount` slots.
  - `batch` (`batch.go`) — in-game intrinsic scenes (`Format == "intrinsic"`);
    fills `SlotCount` surfaces, one advertiser + one category per scene, first-price.
  - `timeslot` (`timeslot.go`) — DOOH rotation slot; delegates to single-winner
    (the audience multiplier is applied downstream at proof-of-play, not here).
- `SeparationContext` / `FilterBidsWithSeparation` (`separation.go`) — per-page
  competitive-separation helper.

## Invariants & Gotchas

- **First-price is the default.** `PriceMode == "second_price"` is implemented in
  `single.go` (second-highest + $0.01, capped at winner's bid) but the exchange
  hardcodes `PriceMode: "first_price"` (`cmd/exchange/main.go`). Bid shading is
  DSP-side (`pkg/bidshading`, imported by `cmd/dsp`) — bids arrive here already shaded.
- **Retail clears generalized second-price**, not first-price: each slot pays the
  minimum bid holding its rank (`next score / own relevance`), floored, capped at
  its own bid. The doc comment in `relevance.go` claiming "first-price, GSP is a
  future refinement" is stale — the code is the truth.
- **Deal priority is NOT here.** `cmd/exchange` runs `pkg/deals` Match/Decide
  BEFORE `RunAuction`: a PG deal preempts the auction entirely, deal floors filter
  bids. The engine only sees post-deal eligible bids (with `Bid.DealID` stamped).
- **Multi-winner money:** the exchange emits ONE AuctionWinEvent per winner, each
  on its own per-surface sub-trace (`surfaceTrace`, `::s{n}` suffix), so every
  surface/slot bills independently. Ignore the stale "billing binds to position 1
  / documented follow-up" comments earlier in `cmd/exchange/main.go`.
- `PodStrategy` and `SeparationContext` have no production caller today — only
  unit tests (the exchange never builds a `PodRequest`). SSAI ad breaks are
  filled per-slot via back-to-back single auctions (`cmd/ssai` `fillBreak`).
- `Bid.SettlementSeat` is the TRUSTED endpoint-bound seat set by the exchange —
  carried through untouched, never part of auction math. Same for `AdomainHost`.
- Pure and deterministic: takes `pkg/clock.Clock` (fake-able), no goroutines, no
  network. Keep it that way — this runs inside the auction hot path.
- Ties in pod/retail/batch sorts use `sort.SliceStable` + explicit tiebreaks
  (raw price); don't replace with unstable sorts.

## Used By

- `cmd/exchange` — the only service importer (`auctionHandler` in `main.go`).

## Testing

Plain unit tests (`go test ./pkg/auction/`), no tags, fake clock. Live coverage
comes via the exchange e2e suites (multi-winner money paths per channel).

## PLAN.md Pointers

`docs/PLAN.md` → "First-Price Auction (Default)" (contains the Unified Auction
Engine spec), "Bid Shading", "Competitive Separation", "Ad Pods (Multiple Ads
per Break)", "Digital Out-of-Home (DOOH)", "Retail Media", "In-Game Advertising".
