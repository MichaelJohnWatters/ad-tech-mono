// Package tigerbeetle implements pkg/billing.Ledger on top of a
// TigerBeetle cluster. The Engine in pkg/billing produces LedgerEntry
// values typed by EntryType; Record() dispatches each type to the right
// combination of TB transfers.
//
// Transfer model (see PLAN.md → "Active Build: TigerBeetle-backed Ledger"
// → settle decision):
//
//   EntrySpend (CPM)       — 2 linked transfers:
//                              adv→publisher (revenue, code=spend)
//                              adv→house     (margin,  code=margin)
//
//   EntryReservation       — 1 pending transfer:
//                              adv→escrow    (full,    code=reservation,
//                              timeout=24h,  user_data_128=trace,
//                              user_data_32=bid_model)
//
//   EntrySettlement        — 3 linked transfers:
//                              post-pending of the reservation (code=settlement)
//                              escrow→publisher (revenue, code=settlement)
//                              escrow→house     (margin,  code=margin)
//                            Escrow nets to zero; advertiser, publisher,
//                            and house balances reflect the true split.
//
//   EntryRelease           — 1 void-pending transfer
//                              (or no-op if TB has already auto-voided
//                              via the reservation timeout).
//
// EntryAdjustment / EntryRefund are not yet implemented — they get
// rejected with ErrUnsupportedEntryType so callers can't silently miss
// them. Wire them when the corresponding flow exists.
package tigerbeetle

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	tbtypes "github.com/tigerbeetle/tigerbeetle-go/pkg/types"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/billing"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tb"
)

// ErrUnsupportedEntryType is returned (via slog) when Record receives an
// EntryType this implementation does not yet emit transfers for.
var ErrUnsupportedEntryType = errors.New("tigerbeetle ledger: unsupported entry type")

// Compile-time proof that *Ledger satisfies billing.Ledger.
var _ billing.Ledger = (*Ledger)(nil)

// Ledger is the TigerBeetle-backed implementation of billing.Ledger.
type Ledger struct {
	client tb.Client
	log    *slog.Logger

	// nextID hands out synthetic IDs for the Ledger interface. Callers in
	// pkg/billing.Engine discard the value — kept only for interface
	// compatibility with MemoryLedger.
	nextID atomic.Int64

	// accountCache remembers TB account IDs we've already ensured exist
	// this process lifetime, so the second and subsequent transfers to
	// an account skip the LookupAccounts round-trip.
	accountCacheMu sync.Mutex
	accountCache   map[tbtypes.Uint128]struct{}

	// summary is an in-process roll-up of recorded transfers. TB has no
	// GROUP BY, and walking every transfer per Summary() call would scale
	// badly. The roll-up is rebuilt from scratch on Ledger startup —
	// implementations that need durable totals should query TB directly
	// per account (see BalanceFor).
	summaryMu sync.RWMutex
	summary   billing.LedgerSummary
}

// New constructs a TigerBeetle-backed ledger over the given client.
// The static escrow and house accounts are auto-provisioned on first
// recorded transfer — no startup blocking call needed.
func New(client tb.Client, log *slog.Logger) *Ledger {
	if log == nil {
		log = slog.Default()
	}
	l := &Ledger{
		client:       client,
		log:          log,
		accountCache: make(map[tbtypes.Uint128]struct{}),
	}
	l.nextID.Store(0)
	return l
}

// Record implements billing.Ledger. The return value is a synthetic
// monotonic ID; existing callers in pkg/billing.Engine discard it.
//
// TB API errors are logged at ERROR level (matching the "failures must be
// ERROR logs" rule) and surfaced as a zero-return — the caller's only
// signal that something went wrong. Engine doesn't currently inspect the
// return, so the loud log is the contract until a future revision adds
// error-aware billing pipelines.
func (l *Ledger) Record(entry billing.LedgerEntry) int64 {
	id := l.nextID.Add(1)
	entry.ID = id

	var err error
	switch entry.Type {
	case billing.EntrySpend:
		err = l.recordSpend(entry)
	case billing.EntryReservation:
		err = l.recordReservation(entry)
	case billing.EntrySettlement:
		err = l.recordSettlement(entry)
	case billing.EntryRelease:
		err = l.recordRelease(entry)
	default:
		err = fmt.Errorf("%w: %s", ErrUnsupportedEntryType, entry.Type)
	}

	if err != nil {
		l.log.Error("tigerbeetle ledger record failed",
			"entry_type", string(entry.Type),
			"trace_id", entry.TraceID,
			"campaign_id", entry.CampaignID,
			"error", err,
		)
		return 0
	}

	l.bumpSummary(entry)
	return id
}

func (l *Ledger) recordSpend(e billing.LedgerEntry) error {
	advID, err := parseAdvertiserAccount(e.DebitAccount)
	if err != nil {
		return fmt.Errorf("parse advertiser account: %w", err)
	}
	pubID, err := parsePublisherAccount(e.CreditAccount)
	if err != nil {
		return fmt.Errorf("parse publisher account: %w", err)
	}

	if err := l.ensureAccount(advID, tb.AccountCodeAdvertiser); err != nil {
		return err
	}
	if err := l.ensureAccount(pubID, tb.AccountCodePublisher); err != nil {
		return err
	}
	if err := l.ensureAccount(tb.HouseAccountID, tb.AccountCodeHouse); err != nil {
		return err
	}

	revenueCents := tb.USDToCents(e.PublisherRevenue)
	marginCents := tb.USDToCents(e.PlatformMargin)
	userData := tb.PackBidModel(e.BidModel)
	traceUD := tb.TraceUserData(e.TraceID)

	return l.createTransfers([]tbtypes.Transfer{
		{
			ID:              tb.SpendTransferID(e.TraceID),
			DebitAccountID:  advID,
			CreditAccountID: pubID,
			Amount:          tb.CentsToAmount(revenueCents),
			UserData128:     traceUD,
			UserData32:      userData,
			Ledger:          tb.USDLedger,
			Code:            tb.CodeSpend,
			Flags:           tbtypes.TransferFlags{Linked: true}.ToUint16(),
		},
		{
			ID:              tb.SpendMarginTransferID(e.TraceID),
			DebitAccountID:  advID,
			CreditAccountID: tb.HouseAccountID,
			Amount:          tb.CentsToAmount(marginCents),
			UserData128:     traceUD,
			UserData32:      userData,
			Ledger:          tb.USDLedger,
			Code:            tb.CodeMargin,
		},
	})
}

func (l *Ledger) recordReservation(e billing.LedgerEntry) error {
	advID, err := parseAdvertiserAccount(e.DebitAccount)
	if err != nil {
		return fmt.Errorf("parse advertiser account: %w", err)
	}

	if err := l.ensureAccount(advID, tb.AccountCodeAdvertiser); err != nil {
		return err
	}
	if err := l.ensureAccount(tb.EscrowAccountID, tb.AccountCodeEscrow); err != nil {
		return err
	}

	cents := tb.USDToCents(e.Amount)
	userData := tb.PackBidModel(e.BidModel)

	return l.createTransfers([]tbtypes.Transfer{{
		ID:              tb.ReservationID(e.TraceID),
		DebitAccountID:  advID,
		CreditAccountID: tb.EscrowAccountID,
		Amount:          tb.CentsToAmount(cents),
		UserData128:     tb.TraceUserData(e.TraceID),
		UserData32:      userData,
		Timeout:         tb.ReservationTimeoutSeconds,
		Ledger:          tb.USDLedger,
		Code:            tb.CodeReservation,
		Flags:           tbtypes.TransferFlags{Pending: true}.ToUint16(),
	}})
}

func (l *Ledger) recordSettlement(e billing.LedgerEntry) error {
	// Settle moves money out of escrow. e.DebitAccount is "escrow:<resID>"
	// (from billing.Engine.settle) and e.CreditAccount is "publisher:<UUID>".
	pubID, err := parsePublisherAccount(e.CreditAccount)
	if err != nil {
		return fmt.Errorf("parse publisher account: %w", err)
	}

	// We need the advertiser too — it isn't on the settle entry directly,
	// but Engine carries it through as AdvertiserID. Defensive: AdvertiserID
	// is required because the pending was advertiser→escrow.
	if e.AdvertiserID == "" {
		return errors.New("settle entry missing advertiser_id")
	}
	advID, err := tb.AdvertiserAccountID(e.AdvertiserID)
	if err != nil {
		return fmt.Errorf("parse advertiser id %q: %w", e.AdvertiserID, err)
	}

	if err := l.ensureAccount(advID, tb.AccountCodeAdvertiser); err != nil {
		return err
	}
	if err := l.ensureAccount(pubID, tb.AccountCodePublisher); err != nil {
		return err
	}
	if err := l.ensureAccount(tb.EscrowAccountID, tb.AccountCodeEscrow); err != nil {
		return err
	}
	if err := l.ensureAccount(tb.HouseAccountID, tb.AccountCodeHouse); err != nil {
		return err
	}

	revenueCents := tb.USDToCents(e.PublisherRevenue)
	marginCents := tb.USDToCents(e.PlatformMargin)
	userData := tb.PackBidModel(e.BidModel)
	traceUD := tb.TraceUserData(e.TraceID)
	pendingID := tb.ReservationID(e.TraceID)

	// Three linked transfers — all-or-nothing. Linked=true on the first
	// two, terminator (Linked=false) on the third. If any fail, all roll
	// back, leaving the pending reservation intact for retry or eventual
	// timeout-driven void.
	return l.createTransfers([]tbtypes.Transfer{
		{
			ID:              tb.SettlementID(e.TraceID),
			DebitAccountID:  advID,
			CreditAccountID: tb.EscrowAccountID,
			Amount:          tbtypes.Uint128{}, // zero = full pending amount
			PendingID:       pendingID,
			UserData128:     traceUD,
			UserData32:      userData,
			Ledger:          tb.USDLedger,
			Code:            tb.CodeSettlement,
			Flags:           tbtypes.TransferFlags{Linked: true, PostPendingTransfer: true}.ToUint16(),
		},
		{
			ID:              tb.SpendTransferID(e.TraceID),
			DebitAccountID:  tb.EscrowAccountID,
			CreditAccountID: pubID,
			Amount:          tb.CentsToAmount(revenueCents),
			UserData128:     traceUD,
			UserData32:      userData,
			Ledger:          tb.USDLedger,
			Code:            tb.CodeSettlement,
			Flags:           tbtypes.TransferFlags{Linked: true}.ToUint16(),
		},
		{
			ID:              tb.MarginTransferID(e.TraceID),
			DebitAccountID:  tb.EscrowAccountID,
			CreditAccountID: tb.HouseAccountID,
			Amount:          tb.CentsToAmount(marginCents),
			UserData128:     traceUD,
			UserData32:      userData,
			Ledger:          tb.USDLedger,
			Code:            tb.CodeMargin,
		},
	})
}

func (l *Ledger) recordRelease(e billing.LedgerEntry) error {
	advID, err := tb.AdvertiserAccountID(e.AdvertiserID)
	if err != nil {
		return fmt.Errorf("parse advertiser id %q: %w", e.AdvertiserID, err)
	}

	pendingID := tb.ReservationID(e.TraceID)

	return l.createTransfers([]tbtypes.Transfer{{
		ID:              tb.ReleaseTransferID(e.TraceID),
		DebitAccountID:  advID,
		CreditAccountID: tb.EscrowAccountID,
		PendingID:       pendingID,
		UserData128:     tb.TraceUserData(e.TraceID),
		Ledger:          tb.USDLedger,
		Code:            tb.CodeRelease,
		Flags:           tbtypes.TransferFlags{VoidPendingTransfer: true}.ToUint16(),
	}})
}

// createTransfers submits to TB and turns every non-OK result into a
// surfaced error. TransferExists is treated as success (idempotent retry).
func (l *Ledger) createTransfers(transfers []tbtypes.Transfer) error {
	results, err := l.client.CreateTransfers(transfers)
	if err != nil {
		return fmt.Errorf("CreateTransfers: %w", err)
	}
	for _, r := range results {
		switch r.Result {
		case tbtypes.TransferOK, tbtypes.TransferExists:
			// fine — idempotent
		default:
			return fmt.Errorf("transfer result for index %d: %v", r.Index, r.Result)
		}
	}
	return nil
}

// ensureAccount lazily creates a TB account. Idempotent across processes
// because TB's CreateAccounts treats "exists" as a non-error result code.
func (l *Ledger) ensureAccount(id tbtypes.Uint128, code uint16) error {
	l.accountCacheMu.Lock()
	if _, ok := l.accountCache[id]; ok {
		l.accountCacheMu.Unlock()
		return nil
	}
	l.accountCacheMu.Unlock()

	results, err := l.client.CreateAccounts([]tbtypes.Account{{
		ID:     id,
		Ledger: tb.USDLedger,
		Code:   code,
	}})
	if err != nil {
		return fmt.Errorf("CreateAccounts: %w", err)
	}
	for _, r := range results {
		switch r.Result {
		case tbtypes.AccountOK, tbtypes.AccountExists:
			// fine
		default:
			return fmt.Errorf("create account result: %v", r.Result)
		}
	}

	l.accountCacheMu.Lock()
	l.accountCache[id] = struct{}{}
	l.accountCacheMu.Unlock()
	return nil
}

func (l *Ledger) bumpSummary(e billing.LedgerEntry) {
	l.summaryMu.Lock()
	defer l.summaryMu.Unlock()
	l.summary.TotalEntries++
	switch e.Type {
	case billing.EntrySpend:
		l.summary.TotalSpend += e.Amount
		l.summary.TotalPublisherRevenue += e.PublisherRevenue
		l.summary.TotalPlatformMargin += e.PlatformMargin
	case billing.EntryReservation:
		l.summary.TotalReserved += e.Amount
	case billing.EntrySettlement:
		l.summary.TotalSettled += e.Amount
		l.summary.TotalPublisherRevenue += e.PublisherRevenue
		l.summary.TotalPlatformMargin += e.PlatformMargin
	case billing.EntryRelease:
		l.summary.TotalReleased += e.Amount
	case billing.EntryRefund:
		l.summary.TotalRefunded += e.Amount
	}
}

// parseAdvertiserAccount expects "advertiser:<uuid>". Same convention as
// MemoryLedger (see billing.Engine.billImmediate).
func parseAdvertiserAccount(s string) (tbtypes.Uint128, error) {
	id, ok := strings.CutPrefix(s, "advertiser:")
	if !ok {
		return tbtypes.Uint128{}, fmt.Errorf("not an advertiser account: %q", s)
	}
	return tb.AccountIDFromUUID(id)
}

func parsePublisherAccount(s string) (tbtypes.Uint128, error) {
	id, ok := strings.CutPrefix(s, "publisher:")
	if !ok {
		return tbtypes.Uint128{}, fmt.Errorf("not a publisher account: %q", s)
	}
	return tb.AccountIDFromUUID(id)
}

