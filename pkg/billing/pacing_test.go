package billing

import (
	"context"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
)

func newPacingEngine(t *testing.T, clk clock.Clock) *Engine {
	t.Helper()
	return NewEngine(NewMemoryLedger(), NewContractStore(), clk, logger.New("pacing-test"))
}

// CPM bills immediately on impression, so committed == the clearing price the
// moment the impression lands.
func TestPacing_CPMImmediate(t *testing.T) {
	clk := clock.NewFake(time.Now())
	e := newPacingEngine(t, clk)

	_, err := e.ProcessEvent(context.Background(), SpendEvent{
		TraceID: "t1", CampaignID: "camp-a", AdvertiserID: "adv-1",
		ClearingPrice: 2.50, Currency: "USD", BidModel: BidCPM, EventType: "impression",
	})
	if err != nil {
		t.Fatalf("process: %v", err)
	}

	snap := e.SnapshotCommitted()
	if got := snap["camp-a"]; got != 2500000 {
		t.Fatalf("committed micros = %d, want 2500000 ($2.50)", got)
	}
}

// CPC reserves on impression (committed = price) and settles on click. Settle
// must be net-neutral: the hold becomes settled, committed stays at the price,
// not double-counted.
func TestPacing_CPCReserveThenSettle(t *testing.T) {
	clk := clock.NewFake(time.Now())
	e := newPacingEngine(t, clk)
	ctx := context.Background()

	// impression → reserve
	if _, err := e.ProcessEvent(ctx, SpendEvent{
		TraceID: "t1", CampaignID: "camp-a", AdvertiserID: "adv-1",
		ClearingPrice: 1.00, Currency: "USD", BidModel: BidCPC, EventType: "impression",
	}); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if got := e.SnapshotCommitted()["camp-a"]; got != 1000000 {
		t.Fatalf("after reserve committed = %d, want 1000000 ($1.00)", got)
	}

	// click → settle
	if _, err := e.ProcessEvent(ctx, SpendEvent{
		TraceID: "t1", CampaignID: "camp-a", AdvertiserID: "adv-1",
		ClearingPrice: 1.00, Currency: "USD", BidModel: BidCPC, EventType: "click",
	}); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if got := e.SnapshotCommitted()["camp-a"]; got != 1000000 {
		t.Fatalf("after settle committed = %d, want 1000000 (net-neutral)", got)
	}
}

// A CPC impression that never gets a click leaves an open reserve. After the
// hold TTL the sweep releases it, so pacing stops counting an impression whose
// billable event never arrived (mirrors the phantom-win release the DSP needs).
func TestPacing_ReserveExpiryReleases(t *testing.T) {
	clk := clock.NewFake(time.Now())
	e := newPacingEngine(t, clk)
	e.SetPacingHoldTTL(5 * time.Minute)

	if _, err := e.ProcessEvent(context.Background(), SpendEvent{
		TraceID: "t1", CampaignID: "camp-a", AdvertiserID: "adv-1",
		ClearingPrice: 3.00, Currency: "USD", BidModel: BidCPC, EventType: "impression",
	}); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if got := e.SnapshotCommitted()["camp-a"]; got != 3000000 {
		t.Fatalf("before expiry committed = %d, want 3000000 ($3.00)", got)
	}

	clk.Advance(6 * time.Minute)
	if released := e.SweepExpiredHolds(); released != 1 {
		t.Fatalf("released = %d, want 1", released)
	}
	snap := e.SnapshotCommitted()
	// camp-a must be PRESENT at 0 (touched today) — not omitted — so the DSP
	// reconciles its counter DOWN instead of leaving it stuck at 300.
	if v, ok := snap["camp-a"]; !ok || v != 0 {
		t.Fatalf("after expiry snapshot[camp-a] = (%d, present=%v), want (0, true)", v, ok)
	}
}

// A settle that arrives after its hold was swept still counts the realized
// spend — the sweep is about releasing budget, not erasing a real charge.
func TestPacing_SettleAfterSweepStillCounts(t *testing.T) {
	clk := clock.NewFake(time.Now())
	e := newPacingEngine(t, clk)
	e.SetPacingHoldTTL(1 * time.Minute)
	ctx := context.Background()

	e.ProcessEvent(ctx, SpendEvent{
		TraceID: "t1", CampaignID: "camp-a", AdvertiserID: "adv-1",
		ClearingPrice: 2.00, Currency: "USD", BidModel: BidCPC, EventType: "impression",
	})
	clk.Advance(2 * time.Minute)
	e.SweepExpiredHolds() // hold released
	if got := e.SnapshotCommitted()["camp-a"]; got != 0 {
		t.Fatalf("after sweep committed = %d, want 0", got)
	}

	// late click settles the (already-swept) reservation
	e.ProcessEvent(ctx, SpendEvent{
		TraceID: "t1", CampaignID: "camp-a", AdvertiserID: "adv-1",
		ClearingPrice: 2.00, Currency: "USD", BidModel: BidCPC, EventType: "click",
	})
	if got := e.SnapshotCommitted()["camp-a"]; got != 2000000 {
		t.Fatalf("after late settle committed = %d, want 2000000 ($2.00)", got)
	}
}

// Committed is per-UTC-day; crossing midnight resets it so a snapshot always
// reflects today (aligns with the DSP budget counter's daily TTL rollover).
func TestPacing_DayRollover(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 7, 5, 23, 0, 0, 0, time.UTC))
	e := newPacingEngine(t, clk)

	e.ProcessEvent(context.Background(), SpendEvent{
		TraceID: "t1", CampaignID: "camp-a", AdvertiserID: "adv-1",
		ClearingPrice: 5.00, Currency: "USD", BidModel: BidCPM, EventType: "impression",
	})
	if got := e.SnapshotCommitted()["camp-a"]; got != 5000000 {
		t.Fatalf("day-1 committed = %d, want 5000000 ($5.00)", got)
	}

	clk.Advance(2 * time.Hour) // now 2026-07-06 01:00 UTC
	if got := e.SnapshotCommitted()["camp-a"]; got != 0 {
		t.Fatalf("day-2 committed = %d, want 0 (rolled over)", got)
	}
}

// PacingState exposes settled and open-reserved separately, and HydratePacing
// restores BOTH on a fresh engine — the restart-safety path. committed survives
// a restart (settled resumed, reserves restored as a synthetic hold).
func TestPacing_PersistRoundTrip(t *testing.T) {
	clk := clock.NewFake(time.Now())
	e := newPacingEngine(t, clk)
	ctx := context.Background()

	// CPM immediate → settled; CPC impression → open reserve (hold).
	e.ProcessEvent(ctx, SpendEvent{TraceID: "t1", CampaignID: "camp-a", AdvertiserID: "adv",
		ClearingPrice: 4.00, Currency: "USD", BidModel: BidCPM, EventType: "impression"})
	e.ProcessEvent(ctx, SpendEvent{TraceID: "t2", CampaignID: "camp-a", AdvertiserID: "adv",
		ClearingPrice: 1.00, Currency: "USD", BidModel: BidCPC, EventType: "impression"})

	// committed = 4000000 settled + 1000000 reserved = 5000000 micros ($4 + $1).
	if got := e.SnapshotCommitted()["camp-a"]; got != 5000000 {
		t.Fatalf("committed = %d, want 5000000", got)
	}
	day, settled, reserved := e.PacingState()
	if settled["camp-a"] != 4000000 || reserved["camp-a"] != 1000000 {
		t.Fatalf("pacingState settled=%d reserved=%d, want 4000000/1000000", settled["camp-a"], reserved["camp-a"])
	}

	// Simulate a restart: fresh engine, hydrate settled + reserved.
	e2 := newPacingEngine(t, clk)
	e2.HydratePacing(day, settled, reserved)
	if got := e2.SnapshotCommitted()["camp-a"]; got != 5000000 {
		t.Fatalf("after hydrate committed = %d, want 5000000 (settled AND reserves restored)", got)
	}
	// The restored reserve is a real hold: it sweeps after the TTL.
	e2.SetPacingHoldTTL(1 * time.Minute)
	clk.Advance(2 * time.Minute)
	e2.SweepExpiredHolds()
	if got := e2.SnapshotCommitted()["camp-a"]; got != 4000000 {
		t.Fatalf("after sweep committed = %d, want 4000000 (restored reserve released)", got)
	}
}

// A hydrate carrying a stale (previous-day) day tag is ignored — a restart on a
// new day must start today at zero, not inherit yesterday's total.
func TestPacing_HydrateIgnoresStaleDay(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC))
	e := newPacingEngine(t, clk)
	e.HydratePacing("2026-07-05", map[string]int64{"camp-a": 999}, map[string]int64{"camp-a": 50})
	if got := e.SnapshotCommitted()["camp-a"]; got != 0 {
		t.Fatalf("stale-day hydrate leaked %d, want 0", got)
	}
}

// Only campaigns with real billing activity appear in a snapshot — a campaign
// the engine never saw an event for is absent, so a DSP won't reconcile it to
// zero and wipe its local in-flight win counter.
func TestPacing_SnapshotOmitsUntouchedCampaigns(t *testing.T) {
	clk := clock.NewFake(time.Now())
	e := newPacingEngine(t, clk)

	e.ProcessEvent(context.Background(), SpendEvent{
		TraceID: "t1", CampaignID: "camp-a", AdvertiserID: "adv-1",
		ClearingPrice: 1.00, Currency: "USD", BidModel: BidCPM, EventType: "impression",
	})

	snap := e.SnapshotCommitted()
	if _, ok := snap["camp-a"]; !ok {
		t.Fatalf("camp-a should be present")
	}
	if _, ok := snap["camp-b"]; ok {
		t.Fatalf("camp-b never billed — should be absent")
	}
}
