package billing

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

func TestEngine_CPM_BillImmediate(t *testing.T) {
	ledger := NewMemoryLedger()
	contracts := NewContractStore()
	clk := clock.NewFake(time.Now())
	log := logger.New("billing-test")
	engine := NewEngine(ledger, contracts, clk, log)

	result, err := engine.ProcessEvent(context.Background(), SpendEvent{
		TraceID:       "t1",
		CampaignID:    "c1",
		PublisherID:   "pub1",
		AdvertiserID:  "adv1",
		ClearingPrice: 3.00,
		Currency:      "USD",
		BidModel:      BidCPM,
		EventType:     "impression",
	})

	if err != nil {
		t.Fatal(err)
	}
	if result.Action != "billed" {
		t.Errorf("action = %s, want billed", result.Action)
	}
	if result.AdvertiserSpend != 3.00 {
		t.Errorf("spend = %.2f, want 3.00", result.AdvertiserSpend)
	}
	// Default contract: 20% fee
	if math.Abs(result.PublisherRevenue-2.40) > 0.01 {
		t.Errorf("publisher_rev = %.2f, want 2.40", result.PublisherRevenue)
	}
	if math.Abs(result.PlatformMargin-0.60) > 0.01 {
		t.Errorf("margin = %.2f, want 0.60", result.PlatformMargin)
	}

	// Check ledger
	entries := ledger.Entries()
	if len(entries) != 1 {
		t.Fatalf("ledger entries = %d, want 1", len(entries))
	}
	if entries[0].Type != EntrySpend {
		t.Errorf("entry type = %s, want spend", entries[0].Type)
	}
}

func TestEngine_CPC_ReserveSettle(t *testing.T) {
	ledger := NewMemoryLedger()
	contracts := NewContractStore()
	clk := clock.NewFake(time.Now())
	log := logger.New("billing-test")
	engine := NewEngine(ledger, contracts, clk, log)

	// Step 1: Impression -> reserve
	res1, _ := engine.ProcessEvent(context.Background(), SpendEvent{
		TraceID: "t2", CampaignID: "c1", PublisherID: "pub1", AdvertiserID: "adv1",
		ClearingPrice: 2.00, Currency: "USD", BidModel: BidCPC, EventType: "impression",
	})
	if res1.Action != "reserved" {
		t.Errorf("action = %s, want reserved", res1.Action)
	}

	// Step 2: Click -> settle
	res2, _ := engine.ProcessEvent(context.Background(), SpendEvent{
		TraceID: "t2", CampaignID: "c1", PublisherID: "pub1", AdvertiserID: "adv1",
		ClearingPrice: 2.00, Currency: "USD", BidModel: BidCPC, EventType: "click",
	})
	if res2.Action != "settled" {
		t.Errorf("action = %s, want settled", res2.Action)
	}
	if res2.PublisherRevenue == 0 {
		t.Error("expected publisher revenue on settle")
	}

	// Ledger should have 2 entries
	if len(ledger.Entries()) != 2 {
		t.Errorf("ledger entries = %d, want 2", len(ledger.Entries()))
	}
}

func TestEngine_SettleByTrace_CPCHappyPath(t *testing.T) {
	ledger := NewMemoryLedger()
	engine := NewEngine(ledger, NewContractStore(), clock.NewFake(time.Now()), logger.New("billing-test"))

	// Reserve via impression on a CPC campaign.
	_, _ = engine.ProcessEvent(context.Background(), SpendEvent{
		TraceID: "trA", CampaignID: "c1", PublisherID: "pub1", AdvertiserID: "adv1",
		ClearingPrice: 2.00, Currency: "USD", BidModel: BidCPC, EventType: "impression",
	})

	// Click arrives — only the trace ID is in hand.
	res, err := engine.SettleByTrace(context.Background(), "trA", "click")
	if err != nil || res == nil {
		t.Fatalf("SettleByTrace: res=%v err=%v", res, err)
	}
	if res.Action != "settled" {
		t.Errorf("action = %s, want settled", res.Action)
	}
	if res.AdvertiserSpend != 2.00 {
		t.Errorf("spend = %.2f, want 2.00 (recovered from reservation)", res.AdvertiserSpend)
	}
}

func TestEngine_SettleByTrace_NoReservationNoOp(t *testing.T) {
	ledger := NewMemoryLedger()
	engine := NewEngine(ledger, NewContractStore(), clock.NewFake(time.Now()), logger.New("billing-test"))

	// CPM impression — no reservation created.
	_, _ = engine.ProcessEvent(context.Background(), SpendEvent{
		TraceID: "trB", CampaignID: "c1", PublisherID: "pub1", AdvertiserID: "adv1",
		ClearingPrice: 3.00, Currency: "USD", BidModel: BidCPM, EventType: "impression",
	})

	res, err := engine.SettleByTrace(context.Background(), "trB", "click")
	if err != nil {
		t.Fatal(err)
	}
	if res != nil {
		t.Errorf("expected no-op (CPM has no reservation), got %+v", res)
	}
}

func TestEngine_SettleByTrace_WrongEventNoOp(t *testing.T) {
	ledger := NewMemoryLedger()
	engine := NewEngine(ledger, NewContractStore(), clock.NewFake(time.Now()), logger.New("billing-test"))

	// CPA campaign reserves at impression — click should NOT settle.
	_, _ = engine.ProcessEvent(context.Background(), SpendEvent{
		TraceID: "trC", CampaignID: "c1", PublisherID: "pub1", AdvertiserID: "adv1",
		ClearingPrice: 5.00, Currency: "USD", BidModel: BidCPA, EventType: "impression",
	})

	res, _ := engine.SettleByTrace(context.Background(), "trC", "click")
	if res != nil {
		t.Errorf("click on CPA must not settle, got %+v", res)
	}

	// Conversion does settle.
	res, _ = engine.SettleByTrace(context.Background(), "trC", "conversion")
	if res == nil || res.Action != "settled" {
		t.Errorf("conversion on CPA: res=%+v, want settled", res)
	}
}

func TestEngine_SettleByTrace_DoubleSettleBlocked(t *testing.T) {
	ledger := NewMemoryLedger()
	engine := NewEngine(ledger, NewContractStore(), clock.NewFake(time.Now()), logger.New("billing-test"))

	_, _ = engine.ProcessEvent(context.Background(), SpendEvent{
		TraceID: "trD", CampaignID: "c1", PublisherID: "pub1", AdvertiserID: "adv1",
		ClearingPrice: 1.50, Currency: "USD", BidModel: BidCPC, EventType: "impression",
	})

	first, _ := engine.SettleByTrace(context.Background(), "trD", "click")
	if first == nil {
		t.Fatal("first settle should succeed")
	}
	second, _ := engine.SettleByTrace(context.Background(), "trD", "click")
	if second != nil {
		t.Errorf("second settle should be a no-op (defends against double-fire), got %+v", second)
	}
}

func TestContract_FixedFee(t *testing.T) {
	c := &Contract{Model: ModelFixed, FeePct: 25}
	rev := c.CalculateRevenue(4.00, "open")
	if math.Abs(rev.PublisherRevenue-3.00) > 0.01 {
		t.Errorf("pub_rev = %.2f, want 3.00", rev.PublisherRevenue)
	}
	if math.Abs(rev.PlatformMargin-1.00) > 0.01 {
		t.Errorf("margin = %.2f, want 1.00", rev.PlatformMargin)
	}
}

func TestContract_TieredFee(t *testing.T) {
	c := &Contract{
		Model: ModelTiered,
		Tiers: []Tier{
			{MinImpressions: 0, MaxImpressions: 1000000, FeePct: 25},
			{MinImpressions: 1000000, MaxImpressions: 10000000, FeePct: 20},
			{MinImpressions: 10000000, MaxImpressions: 0, FeePct: 15},
		},
		MonthImpressions: 5000000, // in tier 2
	}
	rev := c.CalculateRevenue(4.00, "open")
	// Tier 2: 20% fee
	if math.Abs(rev.FeePercent-20) > 0.01 {
		t.Errorf("fee = %.1f%%, want 20%%", rev.FeePercent)
	}
}

func TestContract_GuaranteedMinimum(t *testing.T) {
	c := &Contract{Model: ModelFixed, FeePct: 20, GuaranteedMinCPM: 1.50}

	// Above minimum
	rev1 := c.CalculateRevenue(3.00, "open")
	if rev1.Subsidy != 0 {
		t.Errorf("subsidy = %.2f, want 0 (above minimum)", rev1.Subsidy)
	}

	// Below minimum
	rev2 := c.CalculateRevenue(0.80, "open")
	if rev2.PublisherRevenue != 1.50 {
		t.Errorf("pub_rev = %.2f, want 1.50 (guaranteed)", rev2.PublisherRevenue)
	}
	if rev2.Subsidy <= 0 {
		t.Error("expected subsidy when clearing below minimum")
	}
}

func TestContract_DealTypeModifiers(t *testing.T) {
	c := &Contract{
		Model:  ModelFixed,
		FeePct: 25,
		DealTypeModifiers: map[string]float64{
			"pmp": -5,
			"pg":  -10,
		},
	}

	open := c.CalculateRevenue(4.00, "open")
	pmp := c.CalculateRevenue(4.00, "pmp")
	pg := c.CalculateRevenue(4.00, "pg")

	if open.FeePercent != 25 {
		t.Errorf("open fee = %.0f%%, want 25%%", open.FeePercent)
	}
	if pmp.FeePercent != 20 {
		t.Errorf("pmp fee = %.0f%%, want 20%%", pmp.FeePercent)
	}
	if pg.FeePercent != 15 {
		t.Errorf("pg fee = %.0f%%, want 15%%", pg.FeePercent)
	}
}

func TestLedger_BalanceFor(t *testing.T) {
	ledger := NewMemoryLedger()
	ledger.Record(LedgerEntry{
		Type: EntrySpend, DebitAccount: "advertiser:a1", CreditAccount: "publisher:p1",
		Amount: 10.00, PublisherRevenue: 8.00, PlatformMargin: 2.00,
	})
	ledger.Record(LedgerEntry{
		Type: EntrySpend, DebitAccount: "advertiser:a1", CreditAccount: "publisher:p1",
		Amount: 5.00, PublisherRevenue: 4.00, PlatformMargin: 1.00,
	})

	bal := ledger.BalanceFor("advertiser:a1")
	if bal.TotalDebit != 15.00 {
		t.Errorf("debit = %.2f, want 15.00", bal.TotalDebit)
	}

	summary := ledger.Summary()
	if summary.TotalSpend != 15.00 {
		t.Errorf("total spend = %.2f, want 15.00", summary.TotalSpend)
	}
	if math.Abs(summary.TotalPublisherRevenue-12.00) > 0.01 {
		t.Errorf("total pub rev = %.2f, want 12.00", summary.TotalPublisherRevenue)
	}
}

func TestAttribution_ClickThrough(t *testing.T) {
	engine := NewAttributionEngine(DefaultAttributionWindow())
	now := time.Now()

	engine.RecordTouchPoint(TouchPoint{
		TraceID: "t1", CampaignID: "c1", Type: "impression",
		Timestamp: now.Add(-48 * time.Hour), UserID: "user1",
	})
	engine.RecordTouchPoint(TouchPoint{
		TraceID: "t2", CampaignID: "c1", Type: "click",
		Timestamp: now.Add(-2 * time.Hour), UserID: "user1",
	})

	attr := engine.Attribute("user1", now, 50.00)
	if attr == nil {
		t.Fatal("expected attribution")
	}
	if attr.Type != "click_through" {
		t.Errorf("type = %s, want click_through", attr.Type)
	}
	if attr.TouchPoint.TraceID != "t2" {
		t.Errorf("trace = %s, want t2 (click)", attr.TouchPoint.TraceID)
	}
}

func TestAttribution_ViewThrough(t *testing.T) {
	engine := NewAttributionEngine(DefaultAttributionWindow())
	now := time.Now()

	// Only impression, no click
	engine.RecordTouchPoint(TouchPoint{
		TraceID: "t1", CampaignID: "c1", Type: "impression",
		Timestamp: now.Add(-3 * 24 * time.Hour), UserID: "user1",
	})

	attr := engine.Attribute("user1", now, 25.00)
	if attr == nil {
		t.Fatal("expected view-through attribution")
	}
	if attr.Type != "view_through" {
		t.Errorf("type = %s, want view_through", attr.Type)
	}
}

func TestAttribution_Expired(t *testing.T) {
	engine := NewAttributionEngine(DefaultAttributionWindow())
	now := time.Now()

	// Impression 60 days ago - outside both windows
	engine.RecordTouchPoint(TouchPoint{
		TraceID: "t1", CampaignID: "c1", Type: "impression",
		Timestamp: now.Add(-60 * 24 * time.Hour), UserID: "user1",
	})

	attr := engine.Attribute("user1", now, 10.00)
	if attr != nil {
		t.Error("expected no attribution for expired touchpoint")
	}
}

func TestReconciler_Verify(t *testing.T) {
	store := analytics.NewMemory()
	ledger := NewMemoryLedger()
	ctx := context.Background()
	now := time.Now().UTC()

	// Add matching data
	store.InsertImpression(ctx, &analytics.ImpressionEvent{
		TraceID: "t1", CampaignID: "c1", PlacementID: "p1",
		PublisherID: "pub1", AccountID: "a1",
		ClearingPrice: 2.0, ClearingCurrency: "USD", ClearingPriceUSD: 2.0,
		Timestamp: now,
	})

	ledger.Record(LedgerEntry{
		Type: EntrySpend, CampaignID: "c1", Timestamp: now,
		Amount: 2.0,
	})

	reconciler := NewReconciler(store, ledger)
	result, err := reconciler.Verify(ctx, "c1", now)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Matched {
		t.Errorf("expected matched, got reporting=%d billing=%d", result.ReportingConsumed, result.BillingRecorded)
	}
}

func TestInvoiceGenerator(t *testing.T) {
	ledger := NewMemoryLedger()
	now := time.Now().UTC()

	ledger.Record(LedgerEntry{
		Type: EntrySpend, CampaignID: "c1", Timestamp: now,
		DebitAccount: "advertiser:adv1", CreditAccount: "publisher:pub1",
		Amount: 10.00, PublisherRevenue: 8.00, PlatformMargin: 2.00, Currency: "USD",
	})
	ledger.Record(LedgerEntry{
		Type: EntrySpend, CampaignID: "c2", Timestamp: now,
		DebitAccount: "advertiser:adv1", CreditAccount: "publisher:pub1",
		Amount: 5.00, PublisherRevenue: 4.00, PlatformMargin: 1.00, Currency: "USD",
	})

	gen := NewInvoiceGenerator(ledger)

	inv := gen.GenerateInvoice("adv1", now.AddDate(0, 0, -1), now.AddDate(0, 0, 1), "USD")
	if inv.Total != 15.00 {
		t.Errorf("invoice total = %.2f, want 15.00", inv.Total)
	}
	if len(inv.LineItems) != 2 {
		t.Errorf("line items = %d, want 2", len(inv.LineItems))
	}

	payout := gen.GeneratePayout("pub1", now.AddDate(0, 0, -1), now.AddDate(0, 0, 1), "USD", "net_30")
	if payout.GrossRevenue != 12.00 {
		t.Errorf("payout gross = %.2f, want 12.00", payout.GrossRevenue)
	}
}

// fakeBalanceSink records drawdown calls for assertions.
type fakeBalanceSink struct {
	calls []struct {
		AdvertiserID, EventType string
		Amount                  float64
	}
	err error
}

func (f *fakeBalanceSink) Debit(_ context.Context, advertiserID string, amount float64, _, _, eventType string) (float64, bool, error) {
	f.calls = append(f.calls, struct {
		AdvertiserID, EventType string
		Amount                  float64
	}{advertiserID, eventType, amount})
	return 100 - amount, true, f.err
}

// The prepay drawdown fires exactly at the spend-realization points: CPM
// bill-immediate and reserve/settle's settle — never on the reserve itself.
func TestEngine_BalanceSink_DrawdownPoints(t *testing.T) {
	sink := &fakeBalanceSink{}
	engine := NewEngine(NewMemoryLedger(), NewContractStore(), clock.NewFake(time.Now()), logger.New("billing-test"))
	engine.SetBalanceSink(sink)

	// CPM impression → billed → one debit.
	_, _ = engine.ProcessEvent(context.Background(), SpendEvent{
		TraceID: "bal-1", CampaignID: "c1", PublisherID: "pub1", AdvertiserID: "adv1",
		ClearingPrice: 3.00, Currency: "USD", BidModel: BidCPM, EventType: "impression",
	})
	if len(sink.calls) != 1 || sink.calls[0].Amount != 3.00 || sink.calls[0].AdvertiserID != "adv1" {
		t.Fatalf("CPM bill: sink calls = %+v, want one 3.00 debit for adv1", sink.calls)
	}

	// CPC impression → reserve only → NO debit.
	_, _ = engine.ProcessEvent(context.Background(), SpendEvent{
		TraceID: "bal-2", CampaignID: "c1", PublisherID: "pub1", AdvertiserID: "adv1",
		ClearingPrice: 1.50, Currency: "USD", BidModel: BidCPC, EventType: "impression",
	})
	if len(sink.calls) != 1 {
		t.Fatalf("reserve must not debit the balance; calls = %+v", sink.calls)
	}

	// CPC click → settle → the debit lands.
	_, _ = engine.ProcessEvent(context.Background(), SpendEvent{
		TraceID: "bal-2", CampaignID: "c1", PublisherID: "pub1", AdvertiserID: "adv1",
		ClearingPrice: 1.50, Currency: "USD", BidModel: BidCPC, EventType: "click",
	})
	if len(sink.calls) != 2 || sink.calls[1].Amount != 1.50 || sink.calls[1].EventType != "click" {
		t.Fatalf("settle debit missing/wrong: calls = %+v", sink.calls)
	}
}

// Nil sink (tests, deployments without prepay) and sink errors must never
// fail the billing event — the engine ledger is the source of truth.
func TestEngine_BalanceSink_NilAndErrorTolerated(t *testing.T) {
	engine := NewEngine(NewMemoryLedger(), NewContractStore(), clock.NewFake(time.Now()), logger.New("billing-test"))
	if _, err := engine.ProcessEvent(context.Background(), SpendEvent{
		TraceID: "bal-3", AdvertiserID: "adv1", PublisherID: "pub1",
		ClearingPrice: 2.00, Currency: "USD", BidModel: BidCPM, EventType: "impression",
	}); err != nil {
		t.Fatalf("nil sink must be a no-op, got %v", err)
	}

	engine.SetBalanceSink(&fakeBalanceSink{err: context.DeadlineExceeded})
	if _, err := engine.ProcessEvent(context.Background(), SpendEvent{
		TraceID: "bal-4", AdvertiserID: "adv1", PublisherID: "pub1",
		ClearingPrice: 2.00, Currency: "USD", BidModel: BidCPM, EventType: "impression",
	}); err != nil {
		t.Fatalf("sink error must not fail the billing event, got %v", err)
	}
}

// stringlessLedger wraps MemoryLedger to mimic the TigerBeetle backend: it
// retains the reservation's amount + bid model but DROPS the publisher/
// advertiser/campaign strings on ReservationByTrace — exactly the round-trip
// TB can't do. Used to prove SettleByTrace recovers context from the
// reservation store.
type stringlessLedger struct{ *MemoryLedger }

func (l stringlessLedger) ReservationByTrace(traceID string) (LedgerEntry, bool) {
	e, ok := l.MemoryLedger.ReservationByTrace(traceID)
	if !ok {
		return e, false
	}
	return LedgerEntry{
		TraceID: e.TraceID, Type: e.Type, Amount: e.Amount, Currency: e.Currency,
		BidModel: e.BidModel, ReservationID: e.ReservationID,
		// strings intentionally blanked (TB can't store them)
	}, true
}

// fakeReservationStore is an in-memory billing.ReservationStore.
type fakeReservationStore struct {
	saved map[string]ReservationContext
}

func newFakeReservationStore() *fakeReservationStore {
	return &fakeReservationStore{saved: map[string]ReservationContext{}}
}
func (f *fakeReservationStore) SaveReservation(_ context.Context, rc ReservationContext) error {
	f.saved[rc.TraceID] = rc
	return nil
}
func (f *fakeReservationStore) GetReservation(_ context.Context, traceID string) (ReservationContext, bool, error) {
	rc, ok := f.saved[traceID]
	return rc, ok, nil
}

// The TB settle bug + fix: on a stringless (TB-like) ledger, reserve persists
// the context and settle recovers it — so the settle event carries the
// advertiser/publisher (ledger record succeeds) AND the balance drawdown
// fires (guarded on a non-empty advertiser id).
func TestEngine_ReservationStore_EnrichesSettleOnStringlessLedger(t *testing.T) {
	store := newFakeReservationStore()
	sink := &fakeBalanceSink{}
	engine := NewEngine(stringlessLedger{NewMemoryLedger()}, NewContractStore(), clock.NewFake(time.Now()), logger.New("billing-test"))
	engine.SetReservationStore(store)
	engine.SetBalanceSink(sink)

	// CPC impression → reserve. Context must be persisted.
	_, _ = engine.ProcessEvent(context.Background(), SpendEvent{
		TraceID: "res-1", CampaignID: "camp-1", PublisherID: "pub-1", AdvertiserID: "adv-1",
		ClearingPrice: 2.00, Currency: "USD", BidModel: BidCPC, EventType: "impression",
	})
	if rc, ok := store.saved["res-1"]; !ok || rc.AdvertiserID != "adv-1" || rc.PublisherID != "pub-1" {
		t.Fatalf("reserve did not persist context: %+v", store.saved["res-1"])
	}
	// No drawdown on the reserve itself.
	if len(sink.calls) != 0 {
		t.Fatalf("reserve must not debit the balance; calls=%+v", sink.calls)
	}

	// CPC click → settle. The ledger returns a stringless reservation, so the
	// engine must recover advertiser/publisher from the store.
	res, err := engine.SettleByTrace(context.Background(), "res-1", "click")
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if res == nil || res.Action != "settled" {
		t.Fatalf("settle result = %+v, want a settlement", res)
	}
	// The drawdown fired with the recovered advertiser id — the whole point.
	if len(sink.calls) != 1 || sink.calls[0].AdvertiserID != "adv-1" || sink.calls[0].Amount != 2.00 {
		t.Fatalf("settle drawdown = %+v, want one 2.00 debit for adv-1", sink.calls)
	}
}

// With the MemoryLedger (which retains context), the store is not consulted
// on settle — the ledger's own reservation is complete.
func TestEngine_ReservationStore_MemoryLedgerNeedsNoEnrichment(t *testing.T) {
	store := newFakeReservationStore()
	engine := NewEngine(NewMemoryLedger(), NewContractStore(), clock.NewFake(time.Now()), logger.New("billing-test"))
	engine.SetReservationStore(store)

	_, _ = engine.ProcessEvent(context.Background(), SpendEvent{
		TraceID: "res-2", CampaignID: "camp-2", PublisherID: "pub-2", AdvertiserID: "adv-2",
		ClearingPrice: 1.00, Currency: "USD", BidModel: BidCPC, EventType: "impression",
	})
	// Remove the saved context to prove the memory ledger path doesn't need it.
	delete(store.saved, "res-2")
	res, err := engine.SettleByTrace(context.Background(), "res-2", "click")
	if err != nil || res == nil || res.Action != "settled" {
		t.Fatalf("memory-ledger settle failed without store: res=%+v err=%v", res, err)
	}
}
