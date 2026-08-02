# Data-fee seat integrity — plan

**Status:** building (2026-08-02). Closes the open segtax/data-monetization item:
the external bidder's **self-declared** seat drives the data-fee receivable, so a
bidder can dodge or misdirect what it (or a competitor) owes. Grounded in a
current-state trace (file:line below).

## The vulnerability (confirmed end-to-end)

The seat that the data-monetization **receivable** is billed to comes straight
from the external bidder's OpenRTB response body — a field the bidder fully
controls:

- `cmd/exchange/main.go:1125` — `AdvertiserID: sb.Seat` (copied from the bidder's
  `SeatBid.Seat`) → `:633` `winnerSeat := winnerBid.AdvertiserID` → the response's
  `SeatBid[0].Seat`.
- `cmd/ssp/datafee.go:48` — `seat := bidResp.SeatBid[0].Seat` → `DataFeeEvent.WinnerSeat`.
- `cmd/reporting/datafee.go` — debits `extseat:{seat}:payable` in the ledger +
  writes `data_fee_earnings.winner_seat`.
- `pkg/invoicing/datafee.go:35` — `SUM(...) GROUP BY winner_seat` → one
  `data_fee_receivables` row per seat — the **invoice** key.

So the self-declared string is what an external buyer is actually invoiced under.
Two distinct abuses:

- **Evasion.** The bidder returns an **empty or UUID-shaped** seat. `datafee.go:49`
  (`seat == "" || uuidPattern.MatchString(seat)`) reads that as an *internal*
  winner and returns — **no `DataFeeEvent` is published, the fee is dodged.**
- **Misdirection.** The bidder returns **another partner's** seat string. The
  receivable is billed to the victim; the real buyer pays nothing.

The money math and the impression-time accrual are all correct — the hole is
purely *who* the trusted party is. It's the last open item from the segtax /
ADR-0009 gap review.

## The trusted signal (already present, not used for this)

The exchange fans out to **operator-configured** endpoints
(`exchange.dsp_endpoints`) and assigns each returned bid `DSPID: dspID` +
`endpoint` based on *which endpoint it called* — the bidder can't influence that.
It already distinguishes **internal** (`grpc://` = demand we own, `main.go:1027`)
from **external** (`http://` = third-party). So the winning bid's real owner =
*which configured endpoint answered*, which the exchange knows for a fact. Note
`dspID` is positional (`"dsp-%d"`), so the **endpoint** (or an operator-assigned
seat bound to it) is the stable trusted identity — not `dspID`, and never
`sb.Seat`.

## The fix

Bind a **trusted seat to each external demand endpoint**, operator-controlled,
and bill *that* — never the self-declared response value.

### Phase 1 — Exchange: trusted seat resolution + carry
- `splitDSPEndpoint` already parses a `;notify=<url>` suffix on a `dsp_endpoints`
  entry. Add a sibling `;seat=<id>` suffix → the operator's canonical billable
  seat for that partner (e.g.
  `http://extbidder:9100/bid;seat=acme-dsp;notify=http://extbidder:9100`).
- Build an `endpoint → trusted seat` map (like `dspNotifyBases`). For an
  **external** (`http://`) endpoint the trusted seat = the configured `;seat=`,
  else the endpoint URL (un-forgeable fallback so the fix works before any seat
  is configured). For an **internal** (`grpc://`) endpoint the trusted seat is
  empty (our own demand — data fees out of scope, unchanged).
- Carry the winning bid's trusted seat onto the response:
  `openrtb.BidResponse.SettlementSeat` (new `omitempty` field, an exchange
  extension like `NBR`/`NBRReason`). Empty for internal winners.
- The self-declared `SeatBid[0].Seat` stays as-is for display/other consumers —
  we only add the trusted field; we don't overload the existing one.

### Phase 2 — SSP: attribute the fee to the trusted seat
- `dataFeePublisher.Observe`: use `bidResp.SettlementSeat` as the billable seat.
  **Empty → skip** (internal or non-exchange caller) — this replaces the fragile
  `uuidPattern` heuristic, which was itself the evasion vector.
- If the self-declared `SeatBid[0].Seat` differs from the trusted seat, log a
  **warning** ("data-fee seat mismatch: declared X, trusted Y") — a spoof or
  misconfig signal — but bill the trusted seat.

### Phase 3 — e2e proof
- A `FakeDSP` configured at an endpoint with `;seat=<trusted>` but **self-declaring
  a different seat**: (a) a UUID-shaped seat (evasion attempt) and (b) a
  victim-partner seat (misdirection). Assert `data_fee_earnings.winner_seat` /
  the receivable land on **`<trusted>`**, never the self-declared value, and that
  the evasion attempt still produces an accrual (fee not dodged).
- Internal `grpc://` win on the same fee-bearing segment → **no** data fee
  (unchanged, now decided by the exchange, not a seat-string heuristic).

## Out of scope / notes
- Real cryptographic seat authentication (per-partner signed bids / ads.cert seat
  binding) is a bigger effort; endpoint-bound trusted seats are the honest,
  operator-controlled identity available today and fully close both abuses.
- Diagram: money/event flow is unchanged in shape (same DataFeeEvent →
  receivable), only the seat *source* changes — no `.d2` update needed; the
  SECURITY.md supply-chain table gets the new row.
