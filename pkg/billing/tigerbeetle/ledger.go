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

	// maxPerReq is the adaptive CreateTransfers chunk cap (0 = use the
	// compile-time production default). Shrinks when the server rejects a
	// chunk as oversized (--development mode has a smaller wire limit).
	maxPerReq atomic.Int64

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

	// health feeds the /readyz wedge detector (see health.go).
	health healthState
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

	transfers, err := l.buildTransfers(entry)
	if err == nil {
		err = l.createTransfers(transfers)
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

	l.recordStats([]billing.LedgerEntry{entry})
	l.bumpSummary(entry)
	return id
}

// buildTransfers turns one ledger entry into its TigerBeetle transfer group (a
// self-terminating linked chain for spend/settle, a single pending for reserve,
// a void for release). Splitting build from submit is what lets RecordBatch
// concatenate many entries' groups into one CreateTransfers request.
func (l *Ledger) buildTransfers(entry billing.LedgerEntry) ([]tbtypes.Transfer, error) {
	switch entry.Type {
	case billing.EntrySpend:
		return l.buildSpend(entry)
	case billing.EntryReservation:
		return l.buildReservation(entry)
	case billing.EntrySettlement:
		return l.buildSettlement(entry)
	case billing.EntryRelease:
		return l.buildRelease(entry)
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedEntryType, entry.Type)
	}
}

// maxTransfersPerRequest bounds a single CreateTransfers call. TigerBeetle's
// PRODUCTION wire limit is 8190 transfers/request; stay just under it. Each
// entry's group is <= 3 transfers, and RecordBatch never splits a group across
// this boundary.
//
// The REAL limit is a server config: `tigerbeetle --development` (the local
// stack since 2026-08-05) shrinks message_size_max, and a chunk sized for prod
// gets "Maximum batch size exceeded" — which degraded every flush to the
// per-entry fallback (500 round-trips instead of 1) and crawled the reporting
// impression consumer into a 58k-message backlog mid-soak. The ledger learns
// the actual cap adaptively: on that error it halves a sticky per-process cap
// (perReqCap) and future chunks size to it.
const maxTransfersPerRequest = 8189

// perReqCap is the current adaptive chunk bound (see maxTransfersPerRequest).
func (l *Ledger) perReqCap() int {
	if v := l.maxPerReq.Load(); v > 0 {
		return int(v)
	}
	return maxTransfersPerRequest
}

// shrinkPerReqCap halves the sticky cap after a batch-size rejection of a
// chunk with failedLen transfers. Floor of 3 — an entry's transfer group must
// always fit whole.
func (l *Ledger) shrinkPerReqCap(failedLen int) {
	next := int64(failedLen / 2)
	if next < 3 {
		next = 3
	}
	cur := l.maxPerReq.Load()
	if cur == 0 || next < cur {
		l.maxPerReq.Store(next)
		l.log.Warn("tigerbeetle max batch exceeded — shrinking per-request cap (server likely runs --development)",
			"failed_transfers", failedLen, "new_cap", next)
	}
}

// isBatchSizeExceeded matches TB's oversized-request rejection.
func isBatchSizeExceeded(err error) bool {
	return err != nil && strings.Contains(err.Error(), "batch size exceeded")
}

// RecordBatch submits many entries in as few CreateTransfers requests as
// possible: it builds each entry's transfer group, concatenates groups into
// chunks of <= maxTransfersPerRequest transfers (never splitting a group), and
// submits one request per chunk. On a chunk error it falls back to per-entry
// submission so one poison entry (e.g. a TB linked-chain rejection) can't fail
// the whole batch. Returns each entry's assigned ID in order (0 if its build
// failed). This is the throughput lever: N events → ~1 CreateTransfers.
func (l *Ledger) RecordBatch(entries []billing.LedgerEntry) []int64 {
	ids := make([]int64, len(entries))

	type built struct {
		entry     billing.LedgerEntry
		transfers []tbtypes.Transfer
	}
	chunk := make([]tbtypes.Transfer, 0, 256)
	chunkEntries := make([]built, 0, 128)

	flush := func() {
		if len(chunk) == 0 {
			return
		}
		// Entries whose money transfers committed this flush; their summary
		// buckets are bumped in one stats request at the end.
		var recorded []billing.LedgerEntry
		defer func() { l.recordStats(recorded) }()
		byIdx, err := l.createTransfersResults(chunk)
		if err != nil {
			// Oversized chunk (server's message limit smaller than ours, e.g.
			// --development): learn the real cap so every FUTURE chunk fits in
			// one request; this chunk still drains per-entry below.
			if isBatchSizeExceeded(err) {
				l.shrinkPerReqCap(len(chunk))
			}
			// Transport-level error (the whole request didn't land) — retry each
			// entry alone so a single connection hiccup doesn't drop the batch.
			l.log.Error("tigerbeetle batch chunk transport error; retrying per-entry",
				"entries", len(chunkEntries), "error", err)
			for _, b := range chunkEntries {
				if e2 := l.createTransfers(b.transfers); e2 != nil {
					l.log.Error("tigerbeetle ledger record failed",
						"entry_type", string(b.entry.Type), "trace_id", b.entry.TraceID,
						"campaign_id", b.entry.CampaignID, "error", e2)
					continue
				}
				recorded = append(recorded, b.entry)
				l.bumpSummary(b.entry)
			}
			chunk = chunk[:0]
			chunkEntries = chunkEntries[:0]
			return
		}
		// The request landed. TB commits each independent chain on its own, so
		// classify per-chain from the sparse results instead of re-submitting the
		// whole chunk (which would re-hit already-committed chains as Exists). A
		// chain fails only if one of its transfers has a NON-benign result; a chain
		// whose only non-OK codes are Exists/LinkedEventFailed is an idempotent
		// replay (already recorded) and counts as success.
		idx := 0
		for _, b := range chunkEntries {
			var badResult *tbtypes.TransferEventResult
			for j := idx; j < idx+len(b.transfers); j++ {
				if r, ok := byIdx[j]; ok && !isBenignTransferResult(r) {
					rr := r
					badResult = &rr
					break
				}
			}
			if badResult != nil {
				l.log.Error("tigerbeetle ledger record failed",
					"entry_type", string(b.entry.Type), "trace_id", b.entry.TraceID,
					"campaign_id", b.entry.CampaignID, "result", badResult.Result)
			} else {
				recorded = append(recorded, b.entry)
				l.bumpSummary(b.entry)
			}
			idx += len(b.transfers)
		}
		chunk = chunk[:0]
		chunkEntries = chunkEntries[:0]
	}

	for i := range entries {
		entry := entries[i]
		entry.ID = l.nextID.Add(1)
		transfers, err := l.buildTransfers(entry)
		if err != nil {
			l.log.Error("tigerbeetle ledger record failed",
				"entry_type", string(entry.Type), "trace_id", entry.TraceID,
				"campaign_id", entry.CampaignID, "error", err)
			ids[i] = 0
			continue
		}
		ids[i] = entry.ID
		if len(chunk)+len(transfers) > l.perReqCap() {
			flush()
		}
		chunk = append(chunk, transfers...)
		chunkEntries = append(chunkEntries, built{entry: entry, transfers: transfers})
	}
	flush()
	return ids
}

func (l *Ledger) buildSpend(e billing.LedgerEntry) ([]tbtypes.Transfer, error) {
	advID, err := parseAdvertiserAccount(e.DebitAccount)
	if err != nil {
		return nil, fmt.Errorf("parse advertiser account: %w", err)
	}
	pubID, err := parsePublisherAccount(e.CreditAccount)
	if err != nil {
		return nil, fmt.Errorf("parse publisher account: %w", err)
	}

	if err := l.ensureAccount(advID, tb.AccountCodeAdvertiser, tb.USDLedger); err != nil {
		return nil, err
	}
	if err := l.ensureAccount(pubID, tb.AccountCodePublisher, tb.USDLedger); err != nil {
		return nil, err
	}
	if err := l.ensureAccount(tb.HouseAccountID, tb.AccountCodeHouse, tb.USDLedger); err != nil {
		return nil, err
	}

	revenueMicros := tb.USDToMicros(e.PublisherRevenue)
	marginMicros := tb.USDToMicros(e.PlatformMargin)
	userData := tb.PackBidModel(e.BidModel)
	traceUD := tb.TraceUserData(e.TraceID)

	return []tbtypes.Transfer{
		{
			ID:              tb.SpendTransferID(e.TraceID),
			DebitAccountID:  advID,
			CreditAccountID: pubID,
			Amount:          tb.MicrosToAmount(revenueMicros),
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
			Amount:          tb.MicrosToAmount(marginMicros),
			UserData128:     traceUD,
			UserData32:      userData,
			Ledger:          tb.USDLedger,
			Code:            tb.CodeMargin,
		},
	}, nil
}

func (l *Ledger) buildReservation(e billing.LedgerEntry) ([]tbtypes.Transfer, error) {
	advID, err := parseAdvertiserAccount(e.DebitAccount)
	if err != nil {
		return nil, fmt.Errorf("parse advertiser account: %w", err)
	}

	if err := l.ensureAccount(advID, tb.AccountCodeAdvertiser, tb.USDLedger); err != nil {
		return nil, err
	}
	if err := l.ensureAccount(tb.EscrowAccountID, tb.AccountCodeEscrow, tb.USDLedger); err != nil {
		return nil, err
	}

	cents := tb.USDToMicros(e.Amount)
	userData := tb.PackBidModel(e.BidModel)

	return []tbtypes.Transfer{{
		ID:              tb.ReservationID(e.TraceID),
		DebitAccountID:  advID,
		CreditAccountID: tb.EscrowAccountID,
		Amount:          tb.MicrosToAmount(cents),
		UserData128:     tb.TraceUserData(e.TraceID),
		UserData32:      userData,
		Timeout:         tb.ReservationTimeoutSeconds,
		Ledger:          tb.USDLedger,
		Code:            tb.CodeReservation,
		Flags:           tbtypes.TransferFlags{Pending: true}.ToUint16(),
	}}, nil
}

func (l *Ledger) buildSettlement(e billing.LedgerEntry) ([]tbtypes.Transfer, error) {
	// Settle moves money out of escrow. e.DebitAccount is "escrow:<resID>"
	// (from billing.Engine.settle) and e.CreditAccount is "publisher:<UUID>".
	pubID, err := parsePublisherAccount(e.CreditAccount)
	if err != nil {
		return nil, fmt.Errorf("parse publisher account: %w", err)
	}

	// We need the advertiser too — it isn't on the settle entry directly,
	// but Engine carries it through as AdvertiserID. Defensive: AdvertiserID
	// is required because the pending was advertiser→escrow.
	if e.AdvertiserID == "" {
		return nil, errors.New("settle entry missing advertiser_id")
	}
	advID, err := tb.AdvertiserAccountID(e.AdvertiserID)
	if err != nil {
		return nil, fmt.Errorf("parse advertiser id %q: %w", e.AdvertiserID, err)
	}

	if err := l.ensureAccount(advID, tb.AccountCodeAdvertiser, tb.USDLedger); err != nil {
		return nil, err
	}
	if err := l.ensureAccount(pubID, tb.AccountCodePublisher, tb.USDLedger); err != nil {
		return nil, err
	}
	if err := l.ensureAccount(tb.EscrowAccountID, tb.AccountCodeEscrow, tb.USDLedger); err != nil {
		return nil, err
	}
	if err := l.ensureAccount(tb.HouseAccountID, tb.AccountCodeHouse, tb.USDLedger); err != nil {
		return nil, err
	}

	revenueMicros := tb.USDToMicros(e.PublisherRevenue)
	marginMicros := tb.USDToMicros(e.PlatformMargin)
	userData := tb.PackBidModel(e.BidModel)
	traceUD := tb.TraceUserData(e.TraceID)
	pendingID := tb.ReservationID(e.TraceID)

	// Three linked transfers — all-or-nothing. Linked=true on the first
	// two, terminator (Linked=false) on the third. If any fail, all roll
	// back, leaving the pending reservation intact for retry or eventual
	// timeout-driven void.
	return []tbtypes.Transfer{
		{
			ID:              tb.SettlementID(e.TraceID),
			DebitAccountID:  advID,
			CreditAccountID: tb.EscrowAccountID,
			Amount:          tbtypes.Uint128{}, // zero = full pending amount
			PendingID:       pendingID,
			UserData128:     traceUD,
			UserData32:      userData,
			Ledger:          tb.USDLedger,
			// Code 0 = inherit the pending reservation's code. TigerBeetle
			// rejects a post-pending transfer whose (nonzero) code differs
			// from the pending's (CodeReservation) with
			// pending_transfer_has_different_code — which is exactly what
			// forcing CodeSettlement here did. The "a settlement happened"
			// signal is carried by the escrow→publisher transfer below
			// (CodeSettlement), which HasSettlement / Summary key off, and
			// this posting is still classified as a settlement via its
			// PostPendingTransfer flag (see entryTypeForCode).
			Code:  0,
			Flags: tbtypes.TransferFlags{Linked: true, PostPendingTransfer: true}.ToUint16(),
		},
		{
			ID:              tb.SpendTransferID(e.TraceID),
			DebitAccountID:  tb.EscrowAccountID,
			CreditAccountID: pubID,
			Amount:          tb.MicrosToAmount(revenueMicros),
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
			Amount:          tb.MicrosToAmount(marginMicros),
			UserData128:     traceUD,
			UserData32:      userData,
			Ledger:          tb.USDLedger,
			Code:            tb.CodeMargin,
		},
	}, nil
}

func (l *Ledger) buildRelease(e billing.LedgerEntry) ([]tbtypes.Transfer, error) {
	advID, err := tb.AdvertiserAccountID(e.AdvertiserID)
	if err != nil {
		return nil, fmt.Errorf("parse advertiser id %q: %w", e.AdvertiserID, err)
	}

	pendingID := tb.ReservationID(e.TraceID)

	return []tbtypes.Transfer{{
		ID:              tb.ReleaseTransferID(e.TraceID),
		DebitAccountID:  advID,
		CreditAccountID: tb.EscrowAccountID,
		PendingID:       pendingID,
		UserData128:     tb.TraceUserData(e.TraceID),
		Ledger:          tb.USDLedger,
		Code:            tb.CodeRelease,
		Flags:           tbtypes.TransferFlags{VoidPendingTransfer: true}.ToUint16(),
	}}, nil
}

// createTransfers submits to TB and turns every non-OK result into a
// surfaced error. TransferExists is treated as success (idempotent retry).
// isBenignTransferResult reports whether a per-transfer result code is NOT a
// genuine failure. OK and Exists are the obvious idempotent cases. LinkedEventFailed
// is benign BY ITSELF — it only means "another transfer in my linked chain failed";
// whether that's a real problem is decided by looking at the chain's OTHER results
// (a real error there will not be benign). So a chain whose only non-OK codes are
// Exists + LinkedEventFailed is an idempotent replay (the chain was already applied),
// not a failure — which is exactly what a redelivered event produces.
func isBenignTransferResult(r tbtypes.TransferEventResult) bool {
	switch r.Result {
	case tbtypes.TransferOK, tbtypes.TransferExists, tbtypes.TransferLinkedEventFailed:
		return true
	default:
		return false
	}
}

// createTransfers submits one logical group (a single linked chain, from the
// single-entry Record path). TB returns results only for non-OK transfers, so an
// empty result set is all-OK. A replay yields Exists (+ LinkedEventFailed on its
// chain-mates), all benign — so this returns nil (no-op), preserving idempotency.
func (l *Ledger) createTransfers(transfers []tbtypes.Transfer) error {
	results, err := l.client.CreateTransfers(transfers)
	l.noteTransport(err)
	if err != nil {
		return fmt.Errorf("CreateTransfers: %w", err)
	}
	for _, r := range results {
		if !isBenignTransferResult(r) {
			return fmt.Errorf("transfer result for index %d: %v", r.Index, r.Result)
		}
	}
	return nil
}

// createTransfersResults submits a multi-chain chunk and returns the raw sparse
// result set (one entry per NON-OK transfer, keyed by .Index) so the batch caller
// can classify each independent chain separately. TigerBeetle commits the chains
// that succeed even when a sibling chain fails, so the caller must NOT re-submit
// the whole chunk on any failure (that re-hits committed chains as Exists).
func (l *Ledger) createTransfersResults(transfers []tbtypes.Transfer) (map[int]tbtypes.TransferEventResult, error) {
	results, err := l.client.CreateTransfers(transfers)
	l.noteTransport(err)
	if err != nil {
		return nil, fmt.Errorf("CreateTransfers: %w", err)
	}
	if len(results) == 0 {
		return nil, nil
	}
	byIdx := make(map[int]tbtypes.TransferEventResult, len(results))
	for _, r := range results {
		byIdx[int(r.Index)] = r
	}
	return byIdx, nil
}

// ensureAccount lazily creates a TB account. Idempotent across processes
// because TB's CreateAccounts treats "exists" as a non-error result code.
func (l *Ledger) ensureAccount(id tbtypes.Uint128, code uint16, ledger uint32) error {
	l.accountCacheMu.Lock()
	if _, ok := l.accountCache[id]; ok {
		l.accountCacheMu.Unlock()
		return nil
	}
	l.accountCacheMu.Unlock()

	results, err := l.client.CreateAccounts([]tbtypes.Account{{
		ID:     id,
		Ledger: ledger,
		Code:   code,
	}})
	l.noteTransport(err)
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

