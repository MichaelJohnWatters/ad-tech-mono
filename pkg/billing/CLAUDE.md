# pkg/billing - The Money Engine

Spend calculation, reserve/settle state machine, revenue share, and the double-entry ledger. Not a standalone service — hosted by `cmd/reporting` (the unified billing/reporting binary; `cmd/billing/` is an empty shell whose CLAUDE.md documents the domain). Cost always comes from the AuctionWinEvent's clearing price; this package never recalculates it.

## Key Entry Points

- `Engine` (`billing.go`) — `NewEngine(ledger, contracts, clock, log)`. Per-event: `ProcessEvent` (impressions route by bid model), `SettleByTrace(traceID, eventType)` (clicks/conversions/views/completes — recovers auction context from the reservation, callers don't thread it through tracker params). Batched twins for throughput: `ProcessBatch` / `ProcessSettleBatch` (ONE ledger RecordBatch + ONE DebitBatch + ONE pacing update).
- Bid models: CPM bills immediately; CPC/CPA/vCPM/CPCV **reserve on impression, settle on click/conversion/viewable/complete**. `settleEventMatches` is the single model→trigger mapping.
- Optional wiring (all nil-safe): `SetBalanceSink` (prepay drawdown), `SetReservationStore` (settle context — REQUIRED with TigerBeetle), `SetCommittedCounter` (multi-replica pacing), `SetRateSource` (currency→USD).
- `Ledger` interface (`ledger.go`) — `MemoryLedger` (dev/tests, volatile) or `pkg/billing/tigerbeetle.Ledger` (prod), selected by `billing.ledger_backend` (memory|tigerbeetle; TB addresses via `billing.tigerbeetle_addresses`). Entry types: spend/reservation/settlement/release/adjustment/refund.
- `ContractStore` + `Contract.CalculateRevenue` — publisher revshare (fixed/tiered/guaranteed/deal_type/hybrid); month-to-date impressions merged in at `Get` for tier selection.
- Pacing (`pacing.go`, `committed.go`) — per-campaign committed spend (settled + open reserves) in **micro-dollars**; `SnapshotCommitted`/`SweepExpiredHolds`/`SweepExpiredReservations`/`PacingState`/`HydratePacing`. Reporting broadcasts snapshots on `adtech.billing.campaign_spend_snapshot` (see `cmd/reporting/spend_snapshot.go`; the package itself never touches NATS).
- `InvoiceGenerator` (`invoice.go`), `Reconciler` (`reconciliation.go`), `AttributionEngine` (`attribution.go`).

## CRITICAL Invariants & Gotchas

- **`SpendEvent.ClearingPrice` is realized per-impression dollars, NOT the CPM** — callers convert (`cmd/reporting` normalizeImpressionCost): a $5.00 CPM arrives as $0.005. All committed counters are micros, not cents (cents truncate sub-cent costs to zero).
- **Currency normalizes to USD before ANY money moves.** Non-USD without a rate is dropped loudly (`ErrNoExchangeRate`) — never booked 1:1.
- **Reserves never touch the prepay balance** — only `billImmediate` and settle debit the `BalanceSink`. Sink implementations MUST be idempotent on (traceID, eventType); NATS is at-least-once.
- **TigerBeetle loses the strings.** TB stores only numeric IDs + amount + trace/bid_model, so publisher/advertiser/campaign strings must round-trip through the `ReservationStore` (`pkg/store/postgres.ReservationContextStore`). Same reason the pacing accumulator is built as events flow — an after-the-fact TB query can't reconstruct per-campaign spend.
- **Multi-replica reporting REQUIRES a wired `CommittedCounter`** (additive deltas + periodic store reconcile). N pods publishing partial in-memory snapshots clobber each other on DSP budget keys → overspend. Nil counter = single-replica in-memory behaviour.
- `GuaranteedMinCPM` is CPM-denominated but applied at `/1000` per-impression scale — comparing raw CPM was a 1000× publisher overpay.
- Sweep on the snapshot cadence: `SweepExpiredHolds` frees pacing holds past `reporting.pacing_hold_ttl` (default 15m); `SweepExpiredReservations` reverses memory-ledger escrow (no-op on TB — its 24h pending-transfer timeout auto-voids server-side).
- `SettleByTrace` returning (nil, nil) is normal: no reservation yet (subjects deliver independently), already settled (double-fire guard), or model/event mismatch. Just ack.
- TB backend rejects `EntryAdjustment`/`EntryRefund` (`ErrUnsupportedEntryType`); adjustments live in Postgres.
- `MemoryLedger.Reset` / `Engine.ResetPacing` exist ONLY for the reporting debug billing-reset endpoint (e2e isolation).

## Used By

- `cmd/reporting` — hosts the engine: event consumers (per-event + batch), spend-snapshot publisher (`reporting.spend_snapshot_enabled`/`_interval`), contract warm cache, rate source, trace view. The DSP consumes the snapshots to reconcile pacing (`dsp.spend_reconcile_enabled`) — see `cmd/dsp/CLAUDE.md`.
- `cmd/gateway` — revshare editor shares `billing.Tier` JSON shape.
- `pkg/store/postgres` — `ReservationContextStore`, contract loader.

## Testing

Pure in-process: `MemoryLedger`, `MemoryCommittedCounter`, `pkg/clock` fake — no mocks needed. `pkg/billing/tigerbeetle` has `-tags=integration` tests against a real TB. Money paths get e2e coverage against the live stack (root CLAUDE.md testing rules).

## Architecture Details

See `docs/PLAN.md` -> "Single Source of Truth: The AuctionWinEvent", "How Billing Models Interact with AuctionWinEvent", "Billing and Financial Reconciliation", "Variable Margin and Revenue Share", "Completed Build: TigerBeetle-backed Ledger (2026-06-02)".

## Diagram Updates

If you change this package, check if diagrams need updating:
- **New bid model / settle trigger?** Update `docs/PLAN.md` -> Billing Models + How Billing Models Interact with AuctionWinEvent
- **Changed reserve/settle/release or snapshot flow?** Check the money-flow diagrams via `docs/diagrams/README.md` "Update when" column, then `make diagrams`
- **New ledger entry type or TB transfer shape?** Update `docs/PLAN.md` -> TigerBeetle-backed Ledger section
