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
