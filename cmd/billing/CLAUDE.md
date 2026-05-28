# Billing Service

Consumes AuctionWinEvents from NATS, accrues advertiser spend and publisher revenue. Generates invoices and payouts.

## Responsibilities

- Consume `AuctionWinEvent` from NATS - accrue spend using clearing_price (single source of truth)
- Handle billing models: CPM (bill on win), CPC (reserve on win, settle on click), CPA (reserve on win, settle on conversion)
- Budget reservations for CPC/CPA campaigns
- Release expired reservations (no click/conversion within attribution window)
- Daily reconciliation (verify all consumers processed same event counts)
- Invoice generation (configurable per advertiser: weekly/monthly)
- Payout calculation (publisher revenue minus platform fee)
- Adjustment management (credits/debits for disputes, fraud refunds)

## Key Packages Used

- `pkg/billing/` - spend calculation, invoice models, reconciliation engine
- `pkg/events/` - NATS consumption (AuctionWinEvent, ClickEvent, ConversionEvent)
- `pkg/store/postgres/` - billing tables (with multi-tenancy)

## gRPC Services Exposed

- `BillingService` - see `pkg/proto/`

## CRITICAL: Single Source of Truth

Cost always comes from `AuctionWinEvent.clearing_price`. This service never independently calculates cost. See `docs/PLAN.md` -> "Single Source of Truth: The AuctionWinEvent"

## CRITICAL: CPC/CPA Reserve-Settle Pattern

See `docs/PLAN.md` -> "How Billing Models Interact with AuctionWinEvent"

## Dependencies

- NATS JetStream (consumes auction, click, conversion events)
- Postgres (invoices, payouts, adjustments, reconciliation records, budget reservations)

**Note:** Billing is now unified with the Reporting service (`cmd/reporting`). This CLAUDE.md documents the billing domain logic in `pkg/billing/`, which is hosted by the reporting service binary.

## Diagram Updates

If you change billing logic, check if diagrams need updating:
- **New billing model?** Update `docs/PLAN.md` -> Billing Models + How Billing Models Interact with AuctionWinEvent
- **Changed reconciliation flow?** Update Reconciliation section in PLAN.md
- **New NATS subject consumed?** Update NATS Subjects table
