package tigerbeetle

import (
	"fmt"

	tbtypes "github.com/tigerbeetle/tigerbeetle-go/pkg/types"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/billing"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tb"
)

// queryLimit caps the number of transfers returned per query. TB requires
// a non-zero Limit; this is comfortable for the billing surfaces (a single
// trace produces at most ~5 transfers) without being unbounded.
const queryLimit = 1024

// Entries returns every transfer touching the house account. That captures
// every CPM spend, every settle margin slice, and indirectly tags every
// trace through user_data_128 — sufficient for "recent activity" surfaces.
// Strictly global enumeration is not supported by TB; callers wanting a
// full audit should iterate per-account or query by trace.
func (l *Ledger) Entries() []billing.LedgerEntry {
	return l.entriesForAccountID(tb.HouseAccountID, "platform:house")
}

// EntriesForTrace returns every transfer tagged with the given trace_id
// in user_data_128. Uses QueryTransfers (not GetAccountTransfers) because
// we want every transfer for the trace regardless of which account it
// touches.
func (l *Ledger) EntriesForTrace(traceID string) []billing.LedgerEntry {
	transfers, err := l.client.QueryTransfers(tbtypes.QueryFilter{
		UserData128: tb.TraceUserData(traceID),
		Ledger:      tb.USDLedger,
		Limit:       queryLimit,
	})
	l.noteTransport(err)
	if err != nil {
		l.log.Error("tigerbeetle QueryTransfers failed",
			"trace_id", traceID, "error", err)
		return nil
	}
	out := make([]billing.LedgerEntry, 0, len(transfers))
	for _, t := range transfers {
		out = append(out, transferToEntry(t, traceID))
	}
	return out
}

// EntriesForAccount accepts the same "kind:UUID" strings the MemoryLedger
// uses (advertiser:<uuid>, publisher:<uuid>) plus the static
// "platform:house" / "platform:escrow" sentinels. Anything else returns
// nil to mirror MemoryLedger's silent-empty behaviour.
func (l *Ledger) EntriesForAccount(accountID string) []billing.LedgerEntry {
	tbID, err := resolveAccountID(accountID)
	if err != nil {
		l.log.Debug("tigerbeetle EntriesForAccount: unrecognized account",
			"account_id", accountID, "error", err)
		return nil
	}
	return l.entriesForAccountID(tbID, accountID)
}

// ReservationByTrace looks up the (most recent) pending reservation
// transfer for trace_id. Returns (zero, false) when no reservation exists.
func (l *Ledger) ReservationByTrace(traceID string) (billing.LedgerEntry, bool) {
	transfers, err := l.client.QueryTransfers(tbtypes.QueryFilter{
		UserData128: tb.TraceUserData(traceID),
		Ledger:      tb.USDLedger,
		Code:        tb.CodeReservation,
		Limit:       queryLimit,
		Flags:       tbtypes.QueryFilterFlags{Reversed: true}.ToUint32(),
	})
	l.noteTransport(err)
	if err != nil {
		l.log.Error("tigerbeetle ReservationByTrace QueryTransfers failed",
			"trace_id", traceID, "error", err)
		return billing.LedgerEntry{}, false
	}
	if len(transfers) == 0 {
		return billing.LedgerEntry{}, false
	}
	return transferToEntry(transfers[0], traceID), true
}

// HasSettlement reports whether a CodeSettlement transfer exists for
// trace_id. Used by Engine to guard against double-settle.
func (l *Ledger) HasSettlement(traceID string) bool {
	transfers, err := l.client.QueryTransfers(tbtypes.QueryFilter{
		UserData128: tb.TraceUserData(traceID),
		Ledger:      tb.USDLedger,
		Code:        tb.CodeSettlement,
		Limit:       1,
	})
	l.noteTransport(err)
	if err != nil {
		l.log.Error("tigerbeetle HasSettlement QueryTransfers failed",
			"trace_id", traceID, "error", err)
		return false
	}
	return len(transfers) > 0
}

// BalanceFor uses TB's native account balances: debits_posted and
// credits_posted are accumulators TB maintains atomically.
//
// Semantics differ slightly from MemoryLedger.BalanceFor (which counted
// the full clearing price on both sides of every spend entry, an
// internal quirk). TB returns the actual posted balance: an advertiser's
// TotalDebit is the sum of revenue+margin transfers, which equals the
// clearing price; a publisher's TotalCredit is the sum of revenue slices
// only. The visible API stays the same; downstream consumers that relied
// on memory's double-counting will see corrected values.
func (l *Ledger) BalanceFor(accountID string) billing.BalanceSummary {
	tbID, err := resolveAccountID(accountID)
	if err != nil {
		l.log.Debug("tigerbeetle BalanceFor: unrecognized account",
			"account_id", accountID, "error", err)
		return billing.BalanceSummary{AccountID: accountID}
	}
	accs, err := l.client.LookupAccounts([]tbtypes.Uint128{tbID})
	l.noteTransport(err)
	if err != nil {
		l.log.Error("tigerbeetle BalanceFor LookupAccounts failed",
			"account_id", accountID, "error", err)
		return billing.BalanceSummary{AccountID: accountID}
	}
	if len(accs) == 0 {
		return billing.BalanceSummary{AccountID: accountID}
	}
	a := accs[0]

	debitsPosted, ok := tb.AmountToMicros(a.DebitsPosted)
	if !ok {
		l.log.Error("tigerbeetle BalanceFor: debits exceed uint64",
			"account_id", accountID)
	}
	creditsPosted, _ := tb.AmountToMicros(a.CreditsPosted)
	debitsPending, _ := tb.AmountToMicros(a.DebitsPending)

	return billing.BalanceSummary{
		AccountID:    accountID,
		TotalDebit:   tb.MicrosToUSD(debitsPosted),
		TotalCredit:  tb.MicrosToUSD(creditsPosted),
		Balance:      tb.MicrosToUSD(debitsPosted) - tb.MicrosToUSD(creditsPosted),
		Reservations: tb.MicrosToUSD(debitsPending),
	}
}

// Summary reads the cluster-global stats bucket accounts (see summary.go),
// so every reporting pod answers with the same durable totals — the
// in-process roll-up is only per-pod partial truth at N replicas and is
// kept solely as the fallback when TB is unreachable.
func (l *Ledger) Summary() billing.LedgerSummary {
	s, err := l.summaryFromBuckets()
	if err != nil {
		l.log.Error("tigerbeetle summary bucket read failed, serving in-process fallback",
			"error", err)
		l.summaryMu.RLock()
		defer l.summaryMu.RUnlock()
		return l.summary
	}
	return s
}

// entriesForAccountID is the shared body of Entries and EntriesForAccount.
func (l *Ledger) entriesForAccountID(tbID tbtypes.Uint128, label string) []billing.LedgerEntry {
	transfers, err := l.client.GetAccountTransfers(tbtypes.AccountFilter{
		AccountID: tbID,
		Limit:     queryLimit,
		Flags:     tbtypes.AccountFilterFlags{Debits: true, Credits: true}.ToUint32(),
	})
	l.noteTransport(err)
	if err != nil {
		l.log.Error("tigerbeetle GetAccountTransfers failed",
			"account_label", label, "error", err)
		return nil
	}
	out := make([]billing.LedgerEntry, 0, len(transfers))
	for _, t := range transfers {
		out = append(out, transferToEntry(t, ""))
	}
	return out
}

// transferToEntry projects a TB transfer back into the LedgerEntry shape
// the billing API exposes. Several fields aren't recoverable from TB
// (CampaignID, PublisherID, AdvertiserID as strings) — those come from
// the Postgres warm cache when callers need them. For now they're left
// zero so the entry still serializes.
func transferToEntry(t tbtypes.Transfer, traceID string) billing.LedgerEntry {
	micros, _ := tb.AmountToMicros(t.Amount)
	return billing.LedgerEntry{
		TraceID:       traceID,
		Type:          entryTypeForCode(t.Code, t.TransferFlags()),
		Amount:        tb.MicrosToUSD(micros),
		Currency:      "USD",
		BidModel:      tb.UnpackBidModel(t.UserData32),
		ReservationID: traceID, // best-effort surface; full reservation linkage via PendingID
	}
}

func entryTypeForCode(code uint16, flags tbtypes.TransferFlags) billing.EntryType {
	switch {
	case flags.VoidPendingTransfer:
		return billing.EntryRelease
	case flags.PostPendingTransfer:
		return billing.EntrySettlement
	case code == tb.CodeSpend:
		return billing.EntrySpend
	case code == tb.CodeReservation:
		return billing.EntryReservation
	case code == tb.CodeSettlement:
		return billing.EntrySettlement
	case code == tb.CodeMargin:
		// margin slice — emitted on both CPM spend and settle; safest is
		// to surface as spend so totals reconcile against advertiser debits.
		return billing.EntrySpend
	case code == tb.CodeRelease:
		return billing.EntryRelease
	}
	return billing.EntryAdjustment
}

// resolveAccountID turns the MemoryLedger-style "kind:value" account
// string into a TB account ID. The dispatch covers every account string
// pkg/billing.Engine actually emits.
func resolveAccountID(accountID string) (tbtypes.Uint128, error) {
	switch accountID {
	case "platform:house":
		return tb.HouseAccountID, nil
	case "platform:escrow":
		return tb.EscrowAccountID, nil
	}
	if uuidStr, ok := stripPrefix(accountID, "advertiser:"); ok {
		return tb.AccountIDFromUUID(uuidStr)
	}
	if uuidStr, ok := stripPrefix(accountID, "publisher:"); ok {
		return tb.AccountIDFromUUID(uuidStr)
	}
	if _, ok := stripPrefix(accountID, "escrow:"); ok {
		// MemoryLedger uses per-trace escrow buckets ("escrow:res-<trace>").
		// TB collapses all escrow into a single account — every escrow:foo
		// resolves to the shared escrow ID.
		return tb.EscrowAccountID, nil
	}
	return tbtypes.Uint128{}, fmt.Errorf("unrecognized account id format: %q", accountID)
}

func stripPrefix(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || s[:len(prefix)] != prefix {
		return "", false
	}
	return s[len(prefix):], true
}
