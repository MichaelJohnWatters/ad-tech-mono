package billing

import (
	"sync"
	"time"
)

// EntryType classifies ledger entries.
type EntryType string

const (
	EntrySpend       EntryType = "spend"       // immediate billing (CPM)
	EntryReservation EntryType = "reservation"  // budget hold (CPC/CPA/vCPM/CPCV)
	EntrySettlement  EntryType = "settlement"   // reservation fulfilled
	EntryRelease     EntryType = "release"      // reservation expired/released
	EntryAdjustment  EntryType = "adjustment"   // manual credit/debit
	EntryRefund      EntryType = "refund"       // fraud refund
)

// LedgerEntry is a single double-entry accounting record.
// Every entry has a debit account and a credit account.
// Debits increase spend (advertiser pays), credits increase revenue (publisher earns).
type LedgerEntry struct {
	ID               int64
	Timestamp        time.Time
	TraceID          string
	CampaignID       string
	PublisherID      string
	AdvertiserID     string
	Type             EntryType
	DebitAccount     string  // e.g. "advertiser:adv-acme"
	CreditAccount    string  // e.g. "publisher:pub-daily-news"
	Amount           float64
	PublisherRevenue float64
	PlatformMargin   float64
	Currency         string
	ReservationID    string // links reserve/settle/release
	Description      string
}

// Ledger is an append-only double-entry accounting ledger.
// In production this would be backed by Postgres with ACID guarantees.
type Ledger struct {
	mu      sync.RWMutex
	entries []LedgerEntry
	nextID  int64
}

// NewLedger creates an in-memory ledger.
func NewLedger() *Ledger {
	return &Ledger{nextID: 1}
}

// Record appends an entry to the ledger. Returns the entry ID.
func (l *Ledger) Record(entry LedgerEntry) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry.ID = l.nextID
	l.nextID++
	l.entries = append(l.entries, entry)
	return entry.ID
}

// Entries returns all ledger entries (for debugging/export).
func (l *Ledger) Entries() []LedgerEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]LedgerEntry, len(l.entries))
	copy(out, l.entries)
	return out
}

// EntriesForTrace returns all entries for a given trace ID.
func (l *Ledger) EntriesForTrace(traceID string) []LedgerEntry {
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
func (l *Ledger) EntriesForAccount(accountID string) []LedgerEntry {
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
func (l *Ledger) BalanceFor(accountID string) BalanceSummary {
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
func (l *Ledger) Summary() LedgerSummary {
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
