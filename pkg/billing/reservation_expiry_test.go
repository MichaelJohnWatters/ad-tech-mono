package billing

import (
	"context"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
)

// cpcImpression reserves (does not settle) — CPC bills on the click, so an
// impression only opens the escrow hold.
func cpcImpression(t *testing.T, e *Engine, trace string, price float64) {
	t.Helper()
	if _, err := e.ProcessEvent(context.Background(), SpendEvent{
		TraceID:       trace,
		CampaignID:    "camp-1",
		PublisherID:   "pub-1",
		AdvertiserID:  "adv-1",
		ClearingPrice: price,
		Currency:      "USD",
		BidModel:      BidCPC,
		EventType:     "impression",
	}); err != nil {
		t.Fatalf("reserve %s: %v", trace, err)
	}
}

// TestSweepExpiredReservations covers the memory-ledger reservation release:
// an unsettled reserve older than the TTL is reversed exactly once; a
// not-yet-expired or already-settled reserve is left alone.
func TestSweepExpiredReservations(t *testing.T) {
	clk := clock.NewFake(time.Now())
	ledger := NewMemoryLedger()
	e := NewEngine(ledger, NewContractStore(), clk, logger.New("billing-test"))
	e.SetPacingHoldTTL(10 * time.Minute)

	cpcImpression(t, e, "trace-expire", 0.005)

	if got := ledger.Summary().TotalReserved; got != 0.005 {
		t.Fatalf("TotalReserved after reserve = %v, want 0.005", got)
	}

	// Not yet past the TTL → nothing released.
	clk.Advance(5 * time.Minute)
	if n := e.SweepExpiredReservations(); n != 0 {
		t.Errorf("swept %d before TTL; want 0", n)
	}
	if got := ledger.Summary().TotalReleased; got != 0 {
		t.Errorf("TotalReleased before TTL = %v, want 0", got)
	}

	// Past the TTL → the escrow hold is reversed back to the advertiser.
	clk.Advance(6 * time.Minute) // now 11m > 10m TTL
	if n := e.SweepExpiredReservations(); n != 1 {
		t.Errorf("swept %d after TTL; want 1", n)
	}
	if got := ledger.Summary().TotalReleased; got != 0.005 {
		t.Errorf("TotalReleased after TTL = %v, want 0.005", got)
	}

	// Idempotent: a second sweep must not double-release.
	if n := e.SweepExpiredReservations(); n != 0 {
		t.Errorf("second sweep released %d; want 0 (idempotent)", n)
	}
	if got := ledger.Summary().TotalReleased; got != 0.005 {
		t.Errorf("TotalReleased after re-sweep = %v, want 0.005 (no double-release)", got)
	}
}

// TestSweepExpiredReservationsSkipsSettled proves a reservation that got its
// settle event is never released, even long past the TTL — the click already
// converted the hold to real spend.
func TestSweepExpiredReservationsSkipsSettled(t *testing.T) {
	clk := clock.NewFake(time.Now())
	ledger := NewMemoryLedger()
	e := NewEngine(ledger, NewContractStore(), clk, logger.New("billing-test"))
	e.SetPacingHoldTTL(time.Minute)

	cpcImpression(t, e, "trace-settled", 0.005)
	if _, err := e.SettleByTrace(context.Background(), "trace-settled", "click"); err != nil {
		t.Fatalf("settle: %v", err)
	}

	clk.Advance(time.Hour) // way past the TTL
	if n := e.SweepExpiredReservations(); n != 0 {
		t.Errorf("released a settled reservation (%d); want 0", n)
	}
	s := ledger.Summary()
	if s.TotalSettled != 0.005 {
		t.Errorf("TotalSettled = %v, want 0.005", s.TotalSettled)
	}
	if s.TotalReleased != 0 {
		t.Errorf("TotalReleased = %v, want 0 (settled != released)", s.TotalReleased)
	}
}

// TestSweepExpiredReservationsTTLZeroDisabled: ttl <= 0 releases nothing (the
// sweep is off, matching how a 0 config value disables it).
func TestSweepExpiredReservationsTTLZeroDisabled(t *testing.T) {
	clk := clock.NewFake(time.Now())
	ledger := NewMemoryLedger()
	e := NewEngine(ledger, NewContractStore(), clk, logger.New("billing-test"))
	// holdTTL defaults to a positive value; force the disabled case directly.
	cpcImpression(t, e, "trace-x", 0.005)
	clk.Advance(24 * time.Hour)
	if n := ledger.ReleaseExpired(clk.Now(), 0); n != 0 {
		t.Errorf("ReleaseExpired with ttl=0 released %d; want 0 (disabled)", n)
	}
}
