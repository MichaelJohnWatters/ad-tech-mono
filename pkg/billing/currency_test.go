package billing

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
)

func currencyTestEngine(rs RateSource) (*Engine, *MemoryLedger) {
	ledger := NewMemoryLedger()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e := NewEngine(ledger, NewContractStore(), clock.Real{}, log)
	if rs != nil {
		e.SetRateSource(rs)
	}
	return e, ledger
}

func eurEvent(price float64) SpendEvent {
	return SpendEvent{
		TraceID: "trace-eur-1", CampaignID: "camp-1", PublisherID: "pub-1",
		AdvertiserID: "adv-1", EventType: "impression",
		BidModel: BidCPM, ClearingPrice: price, Currency: "EUR",
	}
}

// A non-USD CPM spend books the USD-converted amount: 0.0035 EUR at
// 0.92 EUR/USD lands as 0.0035/0.92 dollars in the ledger.
func TestCurrencyConversionOnSpend(t *testing.T) {
	e, ledger := currencyTestEngine(func(_ context.Context, cur string, _ time.Time) (float64, bool) {
		if cur == "EUR" {
			return 0.92, true
		}
		return 0, false
	})

	res, err := e.ProcessEvent(context.Background(), eurEvent(0.0035))
	if err != nil {
		t.Fatalf("ProcessEvent: %v", err)
	}
	if res == nil {
		t.Fatal("expected a spend result")
	}
	want := 0.0035 / 0.92
	s := ledger.Summary()
	if s.TotalSpend < want-1e-9 || s.TotalSpend > want+1e-9 {
		t.Errorf("TotalSpend = %v, want %v (EUR converted at 0.92)", s.TotalSpend, want)
	}
	entries := ledger.Entries()
	if len(entries) != 1 || entries[0].Currency != "USD" {
		t.Errorf("ledger entry currency = %+v, want a single USD entry", entries)
	}
}

// USD (and empty-currency) events pass through untouched even with no
// rate source wired — the overwhelmingly common path must not regress.
func TestCurrencyUSDPassthrough(t *testing.T) {
	e, ledger := currencyTestEngine(nil)
	ev := eurEvent(0.0035)
	ev.Currency = "USD"
	if _, err := e.ProcessEvent(context.Background(), ev); err != nil {
		t.Fatalf("USD event must not need a rate source: %v", err)
	}
	ev.Currency = ""
	ev.TraceID = "trace-usd-2"
	if _, err := e.ProcessEvent(context.Background(), ev); err != nil {
		t.Fatalf("empty-currency event must not need a rate source: %v", err)
	}
	if got := ledger.Summary().TotalSpend; got < 0.0069 || got > 0.0071 {
		t.Errorf("TotalSpend = %v, want ~0.007 (two unconverted spends)", got)
	}
}

// An unknown currency is REFUSED — no ledger entry, ErrNoExchangeRate —
// rather than silently booked 1:1 as dollars.
func TestCurrencyUnknownIsRefused(t *testing.T) {
	e, ledger := currencyTestEngine(func(context.Context, string, time.Time) (float64, bool) {
		return 0, false
	})
	ev := eurEvent(0.0035)
	ev.Currency = "XXX"
	_, err := e.ProcessEvent(context.Background(), ev)
	if !errors.Is(err, ErrNoExchangeRate) {
		t.Fatalf("err = %v, want ErrNoExchangeRate", err)
	}
	if got := ledger.Summary().TotalEntries; got != 0 {
		t.Errorf("ledger entries = %d, want 0 (unbillable event must not book)", got)
	}

	// Batch path: the poison event drops, the rest of the batch books.
	usd := eurEvent(0.002)
	usd.Currency = "USD"
	usd.TraceID = "trace-batch-usd"
	bad := eurEvent(0.003)
	bad.Currency = "XXX"
	bad.TraceID = "trace-batch-bad"
	if _, err := e.ProcessBatch(context.Background(), []SpendEvent{usd, bad}); err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}
	s := ledger.Summary()
	if s.TotalEntries != 1 || s.TotalSpend < 0.0019 || s.TotalSpend > 0.0021 {
		t.Errorf("batch summary = %+v, want exactly the USD event booked", s)
	}
}

// Reservations convert too: the escrow hold is in USD, so the later
// settle (which reconstructs from the reservation) is automatically USD.
func TestCurrencyConversionOnReserve(t *testing.T) {
	e, ledger := currencyTestEngine(func(_ context.Context, cur string, _ time.Time) (float64, bool) {
		return 0.92, cur == "EUR"
	})
	ev := eurEvent(0.0035)
	ev.BidModel = BidCPC
	if _, err := e.ProcessEvent(context.Background(), ev); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	want := 0.0035 / 0.92
	if got := ledger.Summary().TotalReserved; got < want-1e-9 || got > want+1e-9 {
		t.Errorf("TotalReserved = %v, want %v", got, want)
	}
}
