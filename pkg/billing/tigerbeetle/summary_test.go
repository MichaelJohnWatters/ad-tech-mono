package tigerbeetle

import (
	"errors"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/billing"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tb"
)

// TestSummaryIsSharedAcrossLedgerInstances is the split-brain regression
// test: reporting runs N replicas, each with its own Ledger over the SAME
// TB cluster, and NATS delivers each billing event to only one of them.
// A Summary() served by any replica must still include entries recorded
// by the others — i.e. it must come from the shared bucket accounts, not
// the per-process accumulator.
func TestSummaryIsSharedAcrossLedgerInstances(t *testing.T) {
	fc := newFakeClient()
	podA := New(fc, silentLogger())
	podB := New(fc, silentLogger())

	adv, pub := advUUID("acme"), pubUUID("dailynews")
	podA.Record(billing.LedgerEntry{
		Type: billing.EntrySpend, TraceID: "trace-shared-1",
		AdvertiserID: adv, PublisherID: pub,
		DebitAccount: "advertiser:" + adv, CreditAccount: "publisher:" + pub,
		Amount: 1.00, PublisherRevenue: 0.80, PlatformMargin: 0.20,
		Currency: "USD", BidModel: "cpm",
	})
	podA.Record(billing.LedgerEntry{
		Type: billing.EntryReservation, TraceID: "trace-shared-2",
		AdvertiserID: adv, DebitAccount: "advertiser:" + adv,
		CreditAccount: "escrow:res-trace-shared-2",
		Amount:        0.0035, Currency: "USD", BidModel: "vcpm",
	})

	// podB recorded nothing, but must see podA's totals.
	s := podB.Summary()
	if s.TotalEntries != 2 {
		t.Errorf("podB TotalEntries = %d, want 2 (recorded by podA)", s.TotalEntries)
	}
	if s.TotalSpend != 1.00 {
		t.Errorf("podB TotalSpend = %v, want 1.00", s.TotalSpend)
	}
	if s.TotalReserved != 0.0035 {
		t.Errorf("podB TotalReserved = %v, want 0.0035", s.TotalReserved)
	}
	if s.TotalPublisherRevenue != 0.80 {
		t.Errorf("podB TotalPublisherRevenue = %v, want 0.80", s.TotalPublisherRevenue)
	}
	if s.TotalPlatformMargin != 0.20 {
		t.Errorf("podB TotalPlatformMargin = %v, want 0.20", s.TotalPlatformMargin)
	}
}

// TestSummaryFallsBackWhenBucketsUnreadable: TB transport failure on the
// bucket lookup must degrade to the in-process accumulator, not zeros.
func TestSummaryFallsBackWhenBucketsUnreadable(t *testing.T) {
	fc := newFakeClient()
	l := New(fc, silentLogger())

	adv, pub := advUUID("acme"), pubUUID("dailynews")
	l.Record(billing.LedgerEntry{
		Type: billing.EntrySpend, TraceID: "trace-fb-1",
		AdvertiserID: adv, PublisherID: pub,
		DebitAccount: "advertiser:" + adv, CreditAccount: "publisher:" + pub,
		Amount: 2.00, PublisherRevenue: 1.60, PlatformMargin: 0.40,
		Currency: "USD", BidModel: "cpm",
	})

	fc.lookupAccountsErr = errors.New("cluster unreachable")
	s := l.Summary()
	if s.TotalSpend != 2.00 {
		t.Errorf("fallback TotalSpend = %v, want 2.00 (in-process accumulator)", s.TotalSpend)
	}
	if s.TotalEntries != 1 {
		t.Errorf("fallback TotalEntries = %d, want 1", s.TotalEntries)
	}
}

// TestStatTransferIDsAreDeterministicPerEntry: a redelivered event must
// re-derive the same stat transfer IDs so TB's Exists result makes the
// replay a no-op — and two entry types on the same trace must NOT collide.
func TestStatTransferIDsAreDeterministicPerEntry(t *testing.T) {
	res := billing.LedgerEntry{Type: billing.EntryReservation, TraceID: "trace-x", Amount: 1}
	set := billing.LedgerEntry{Type: billing.EntrySettlement, TraceID: "trace-x", Amount: 1}

	a1, a2 := statTransfers(res), statTransfers(res)
	for i := range a1 {
		if a1[i].ID != a2[i].ID {
			t.Fatalf("stat transfer %d not deterministic across replays", i)
		}
	}

	// The entries-bucket increment exists for both entry types on the same
	// trace — distinct IDs or the second entry would vanish as a TB replay.
	resEntries := a1[len(a1)-1].ID // entries bucket is appended last
	setEntries := statTransfers(set)[len(statTransfers(set))-1].ID
	if resEntries == setEntries {
		t.Fatal("entries-bucket stat IDs collide across entry types on one trace")
	}

	// All transfers ride the stats ledger with the stat code.
	for _, tr := range a1 {
		if tr.Ledger != tb.StatsLedger || tr.Code != tb.CodeStat {
			t.Fatalf("stat transfer on wrong ledger/code: ledger=%d code=%d", tr.Ledger, tr.Code)
		}
	}
}
