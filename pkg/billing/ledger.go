package billing

import (
	"sync"
	"time"
)

// EntryType classifies ledger entries.
type EntryType string

const (
	EntrySpend       EntryType = "spend"       // immediate billing (CPM)
	EntryReservation EntryType = "reservation" // budget hold (CPC/CPA/vCPM/CPCV)
	EntrySettlement  EntryType = "settlement"  // reservation fulfilled
	EntryRelease     EntryType = "release"     // reservation expired/released
	EntryAdjustment  EntryType = "adjustment"  // manual credit/debit
	EntryRefund      EntryType = "refund"      // fraud refund
)

// LedgerEntry is a single double-entry accounting record.
// Every entry has a debit account and a credit account.
// Debits increase spend (advertiser pays), credits increase revenue (publisher earns).
//
// BidModel + DealType are persisted on the reservation row so that a later
// settle (click for CPC, conversion for CPA, view for vCPM, complete for
// CPCV) can reconstruct the SpendEvent from the reservation alone — the
// settle-side event payload (a click pixel, say) doesn't carry the
// auction's clearing price or the deal type, but the reservation does.
type LedgerEntry struct {
	ID               int64
	Timestamp        time.Time
	TraceID          string
	CampaignID       string
	PublisherID      string
	AdvertiserID     string
	Type             EntryType
	DebitAccount     string // e.g. "advertiser:adv-acme"
	CreditAccount    string // e.g. "publisher:pub-daily-news"
	Amount           float64
	PublisherRevenue float64
	PlatformMargin   float64
	Currency         string
	BidModel         string // cpm, cpc, cpa, vcpm, cpcv — needed at settle time
	DealType         string // open, pmp, pg, preferred — needed for revenue split at settle
	ReservationID    string // links reserve/settle/release
	Description      string
}

// Ledger is the append-only double-entry accounting store the billing
// engine writes to and queries. Implementations:
//
//   - MemoryLedger — in-process slice, used by tests and dev-mode reporting.
//   - pkg/billing/tigerbeetle.Ledger — TigerBeetle-backed, used in prod.
//
// All methods are safe for concurrent use; implementations are responsible
// for their own synchronization.
type Ledger interface {
	// Record appends an entry and returns its assigned ID.
	Record(entry LedgerEntry) int64
	// RecordBatch appends multiple entries in one operation and returns their
	// assigned IDs in order. Equivalent to calling Record for each entry, but
	// backends that batch their durable write (e.g. TigerBeetle) submit all
	// entries' transfers in a single request — the throughput win. MemoryLedger
	// appends them under a single lock.
	RecordBatch(entries []LedgerEntry) []int64
	// Entries returns all entries (for debugging/export).
	Entries() []LedgerEntry
	// EntriesForTrace returns all entries for a given trace ID.
	EntriesForTrace(traceID string) []LedgerEntry
	// EntriesForAccount returns all entries that touch the given account
	// (as either the debit or credit side).
	EntriesForAccount(accountID string) []LedgerEntry
	// ReservationByTrace returns the (most recent) reservation entry for a
	// trace ID, used by Engine.SettleByTrace to reconstruct the original
	// auction context at settle time.
	ReservationByTrace(traceID string) (LedgerEntry, bool)
	// HasSettlement reports whether trace_id has already been settled.
	HasSettlement(traceID string) bool
	// ReleaseExpired records a release for every reservation older than ttl
	// that was never settled (and isn't already released), reversing the escrow
	// hold back to the advertiser — the memory-backend equivalent of
	// TigerBeetle's pending-transfer auto-void. Idempotent across repeated
	// sweeps. now/ttl are passed in so the caller owns the clock. Returns the
	// number released.
	ReleaseExpired(now time.Time, ttl time.Duration) int
	// BalanceFor computes the net balance for an account.
	BalanceFor(accountID string) BalanceSummary
	// Summary returns aggregate billing stats.
	Summary() LedgerSummary
}

// MemoryLedger is the in-process implementation of Ledger. Volatile —
// restart wipes the slice — so production uses pkg/billing/tigerbeetle
// behind the same interface.
type MemoryLedger struct {
	mu      sync.RWMutex
	entries []LedgerEntry
	nextID  int64
}

// NewMemoryLedger creates an empty in-memory ledger.
func NewMemoryLedger() *MemoryLedger {
	return &MemoryLedger{nextID: 1}
}

// Record appends an entry to the ledger. Returns the entry ID.
// Reset wipes all entries and rewinds the auto-increment counter. Used
// by the reporting service's /debug/billing/reset endpoint so e2e tests
// can run billing scenarios in isolation without inheriting ledger state
// from prior tests in the same pod lifetime. Production deployments use
// the TigerBeetle backend instead, which has its own reset story (replace
// the pod, restart from snapshot, etc.) — Reset is in-memory only.
func (l *MemoryLedger) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = nil
	l.nextID = 1
}

func (l *MemoryLedger) Record(entry LedgerEntry) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry.ID = l.nextID
	l.nextID++
	l.entries = append(l.entries, entry)
	return entry.ID
}

// RecordBatch appends all entries under a single lock, assigning sequential
// IDs, and returns them in order. Semantically identical to calling Record for
// each entry.
func (l *MemoryLedger) RecordBatch(entries []LedgerEntry) []int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	ids := make([]int64, len(entries))
	for i := range entries {
		entries[i].ID = l.nextID
		l.nextID++
		l.entries = append(l.entries, entries[i])
		ids[i] = entries[i].ID
	}
	return ids
}

// Entries returns all ledger entries (for debugging/export).
func (l *MemoryLedger) Entries() []LedgerEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]LedgerEntry, len(l.entries))
	copy(out, l.entries)
	return out
}

// ReservationByTrace returns the (most recent) reservation entry for a
// trace ID, used by Engine.SettleByTrace to reconstruct the original
// auction context at settle time. Returns (zero, false) when no
// reservation exists — either the impression was billed immediately
// (CPM) or no impression event has been processed yet.
func (l *MemoryLedger) ReservationByTrace(traceID string) (LedgerEntry, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for i := len(l.entries) - 1; i >= 0; i-- {
		e := l.entries[i]
		if e.TraceID == traceID && e.Type == EntryReservation {
			return e, true
		}
	}
	return LedgerEntry{}, false
}

// HasSettlement reports whether trace_id has already been settled —
// guards against double-settle when an event fires twice and tracker
// dedup doesn't catch it (cross-pod restart, dedup-disabled config, etc).
func (l *MemoryLedger) HasSettlement(traceID string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, e := range l.entries {
		if e.TraceID == traceID && e.Type == EntrySettlement {
			return true
		}
	}
	return false
}

// ReleaseExpired — see the Ledger interface. Reserve never drew the
// advertiser's prepay balance (only settle does), so a release is a pure ledger
// reversal (escrow → advertiser); the pacing hold that counts toward committed
// spend is freed separately by the pacing sweep on the same TTL.
func (l *MemoryLedger) ReleaseExpired(now time.Time, ttl time.Duration) int {
	if ttl <= 0 {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	// A trace that already carries a settlement OR a release is terminal — never
	// release it (again). Building the set up front keeps repeated sweeps
	// idempotent and handles a duplicate reservation for one trace.
	terminal := make(map[string]bool)
	for i := range l.entries {
		if t := l.entries[i].Type; t == EntrySettlement || t == EntryRelease {
			terminal[l.entries[i].TraceID] = true
		}
	}

	// Collect first (can't append to l.entries while ranging it), then append.
	var toRelease []LedgerEntry
	for i := range l.entries {
		e := l.entries[i]
		if e.Type != EntryReservation || terminal[e.TraceID] {
			continue
		}
		if now.Sub(e.Timestamp) < ttl {
			continue
		}
		terminal[e.TraceID] = true
		toRelease = append(toRelease, e)
	}
	for _, e := range toRelease {
		l.entries = append(l.entries, LedgerEntry{
			ID:            l.nextID,
			Timestamp:     now,
			TraceID:       e.TraceID,
			CampaignID:    e.CampaignID,
			PublisherID:   e.PublisherID,
			AdvertiserID:  e.AdvertiserID,
			Type:          EntryRelease,
			DebitAccount:  e.CreditAccount, // escrow:res-…
			CreditAccount: e.DebitAccount,  // advertiser:…
			Amount:        e.Amount,
			Currency:      e.Currency,
			BidModel:      e.BidModel,
			DealType:      e.DealType,
			ReservationID: e.ReservationID,
		})
		l.nextID++
	}
	return len(toRelease)
}

// EntriesForTrace returns all entries for a given trace ID.
func (l *MemoryLedger) EntriesForTrace(traceID string) []LedgerEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var result []LedgerEntry
	for _, e := range l.entries {
		if e.TraceID == traceID {
			result = append(result, e)
		}
	}
	return result
}

// EntriesForAccount returns all entries where the account is debited or credited.
func (l *MemoryLedger) EntriesForAccount(accountID string) []LedgerEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var result []LedgerEntry
	for _, e := range l.entries {
		if e.DebitAccount == accountID || e.CreditAccount == accountID {
			result = append(result, e)
		}
	}
	return result
}

// BalanceFor computes the net balance for an account.
// Debits increase the balance (money owed/spent), credits decrease it (money earned).
func (l *MemoryLedger) BalanceFor(accountID string) BalanceSummary {
	l.mu.RLock()
	defer l.mu.RUnlock()

	var totalDebit, totalCredit, reservations float64
	for _, e := range l.entries {
		if e.DebitAccount == accountID {
			totalDebit += e.Amount
		}
		if e.CreditAccount == accountID {
			totalCredit += e.Amount
		}
		if e.Type == EntryReservation && e.DebitAccount == accountID {
			reservations += e.Amount
		}
		if e.Type == EntrySettlement && e.DebitAccount == accountID {
			// Settlement moves from escrow, reduce reservation
		}
	}

	return BalanceSummary{
		AccountID:    accountID,
		TotalDebit:   totalDebit,
		TotalCredit:  totalCredit,
		Balance:      totalDebit - totalCredit,
		Reservations: reservations,
	}
}

// Summary returns aggregate billing stats.
func (l *MemoryLedger) Summary() LedgerSummary {
	l.mu.RLock()
	defer l.mu.RUnlock()

	var s LedgerSummary
	s.TotalEntries = len(l.entries)
	for _, e := range l.entries {
		switch e.Type {
		case EntrySpend:
			s.TotalSpend += e.Amount
			s.TotalPublisherRevenue += e.PublisherRevenue
			s.TotalPlatformMargin += e.PlatformMargin
		case EntryReservation:
			s.TotalReserved += e.Amount
		case EntrySettlement:
			s.TotalSettled += e.Amount
			s.TotalPublisherRevenue += e.PublisherRevenue
			s.TotalPlatformMargin += e.PlatformMargin
		case EntryRelease:
			s.TotalReleased += e.Amount
		case EntryRefund:
			s.TotalRefunded += e.Amount
		}
	}
	return s
}

// LedgerSummary holds aggregate billing stats.
type LedgerSummary struct {
	TotalEntries          int
	TotalSpend            float64
	TotalReserved         float64
	TotalSettled          float64
	TotalReleased         float64
	TotalRefunded         float64
	TotalPublisherRevenue float64
	TotalPlatformMargin   float64
}
