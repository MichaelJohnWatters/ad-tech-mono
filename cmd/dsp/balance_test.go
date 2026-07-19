package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

func quietBalanceLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func rows(pairs ...any) []postgres.AdvertiserBalance {
	out := []postgres.AdvertiserBalance{}
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, postgres.AdvertiserBalance{
			AccountID: pairs[i].(string), Balance: pairs[i+1].(float64), Currency: "USD",
		})
	}
	return out
}

func TestBalanceGate_GatesOnFunds(t *testing.T) {
	l2 := cache.NewMemoryL2()
	g := NewBalanceGate(l2, nil, quietBalanceLog())
	g.rebase(context.Background(), rows("acct-a", 10.00, "acct-b", 0.0))

	if ok, rem := g.HasFunds("acct-a"); !ok || rem != 10.00 {
		t.Errorf("funded account: ok=%v rem=%v, want true 10.00", ok, rem)
	}
	if ok, _ := g.HasFunds("acct-b"); ok {
		t.Errorf("zero-balance account must not bid")
	}
	// Deliberate fail-closed: no balance row at all = never topped up.
	if ok, _ := g.HasFunds("acct-unknown"); ok {
		t.Errorf("account without a balance row must not bid (prepay)")
	}
}

// An invoiced account with a zero balance may bid on credit up to
// credit_limit, then stops once accrued spend exceeds the limit. Prepay is the
// special case credit_limit=0 (covered by the tests above).
func TestBalanceGate_InvoicedBidsOnCredit(t *testing.T) {
	l2 := cache.NewMemoryL2()
	g := NewBalanceGate(l2, nil, quietBalanceLog())
	// $0 balance, $5.00 credit limit — invoiced.
	g.rebase(context.Background(), []postgres.AdvertiserBalance{{
		AccountID: "inv-a", Balance: 0, Currency: "USD", PaymentTerms: "invoiced", CreditLimit: 5.00,
	}})

	// Zero balance but credit headroom → may bid.
	if ok, rem := g.HasFunds("inv-a"); !ok || rem != 5.00 {
		t.Fatalf("invoiced 0-balance: ok=%v rem=%v, want true 5.00 (credit headroom)", ok, rem)
	}
	// Spend draws the credit down; a positive sliver still bids.
	g.RecordWin("inv-a", 4.80)
	if ok, rem := g.HasFunds("inv-a"); !ok || rem != 0.20 {
		t.Fatalf("invoiced after 4.80 spend: ok=%v rem=%v, want true 0.20", ok, rem)
	}
	// Spend now exceeds the credit limit → stops bidding.
	g.RecordWin("inv-a", 0.40)
	if ok, _ := g.HasFunds("inv-a"); ok {
		t.Fatalf("invoiced past credit limit must stop bidding")
	}
}

// A prepay account (credit_limit 0) with zero balance never bids — the new
// formula reduces to the prepay gate exactly. A missing row still fails closed.
func TestBalanceGate_PrepayZeroCreditUnchanged(t *testing.T) {
	l2 := cache.NewMemoryL2()
	g := NewBalanceGate(l2, nil, quietBalanceLog())
	g.rebase(context.Background(), []postgres.AdvertiserBalance{{
		AccountID: "pre-a", Balance: 0, Currency: "USD", PaymentTerms: "prepay", CreditLimit: 0,
	}})
	if ok, _ := g.HasFunds("pre-a"); ok {
		t.Fatalf("prepay 0-balance 0-credit must not bid")
	}
	if ok, _ := g.HasFunds("pre-unknown"); ok {
		t.Fatalf("account without a balance row must not bid, credit or not")
	}
}

func TestBalanceGate_WinsDrawDownUntilRebase(t *testing.T) {
	l2 := cache.NewMemoryL2()
	g := NewBalanceGate(l2, nil, quietBalanceLog())
	g.rebase(context.Background(), rows("acct-a", 5.00))

	// Wins draw the mirror down; a positive sliver (0.20) still bids —
	// prepay blocks at <= 0, small overshoot is the accepted tolerance.
	g.RecordWin("acct-a", 2.40)
	if ok, rem := g.HasFunds("acct-a"); !ok || rem != 2.60 {
		t.Fatalf("after first win: ok=%v rem=%v, want true 2.60", ok, rem)
	}
	g.RecordWin("acct-a", 2.40)
	if ok, rem := g.HasFunds("acct-a"); !ok || rem != 0.20 {
		t.Fatalf("after second win: ok=%v rem=%v, want true 0.20 (positive sliver still bids)", ok, rem)
	}
	g.RecordWin("acct-a", 2.40)
	if ok, rem := g.HasFunds("acct-a"); ok {
		t.Fatalf("after third win: ok=%v rem=%v, want blocked at <= 0", ok, rem)
	}
}

// The billing drawdown lands in Postgres and the next refresh delivers a
// LOWER balance while the mirror counter still carries the same wins — the
// rebase must not double-count them.
func TestBalanceGate_RebaseDoesNotDoubleCount(t *testing.T) {
	l2 := cache.NewMemoryL2()
	g := NewBalanceGate(l2, nil, quietBalanceLog())
	g.rebase(context.Background(), rows("acct-a", 10.00))

	g.RecordWin("acct-a", 4.00)
	if ok, rem := g.HasFunds("acct-a"); !ok || rem != 6.00 {
		t.Fatalf("pre-rebase: ok=%v rem=%v, want true 6.00", ok, rem)
	}

	// Billing settled the 4.00: snapshot now says 6.00. Counter unchanged.
	g.rebase(context.Background(), rows("acct-a", 6.00))
	if ok, rem := g.HasFunds("acct-a"); !ok || rem != 6.00 {
		t.Errorf("post-rebase: ok=%v rem=%v, want true 6.00 (win must not be counted twice)", ok, rem)
	}
}

func TestBalanceGate_TopupRefreshRestoresBidding(t *testing.T) {
	l2 := cache.NewMemoryL2()
	g := NewBalanceGate(l2, nil, quietBalanceLog())
	g.rebase(context.Background(), rows("acct-a", 1.00))
	g.RecordWin("acct-a", 1.00)
	if ok, _ := g.HasFunds("acct-a"); ok {
		t.Fatal("exhausted account still bidding")
	}
	// Topup lands; the invalidate-triggered refresh delivers the new balance.
	g.rebase(context.Background(), rows("acct-a", 51.00))
	if ok, rem := g.HasFunds("acct-a"); !ok || rem != 51.00 {
		t.Errorf("after topup refresh: ok=%v rem=%v, want true 51.00", ok, rem)
	}
}

// failingL2 errors on Get for the mirror key — the gate must fail OPEN on
// the snapshot alone (bounded staleness), never fail the bid path.
type failingL2 struct{ cache.L2Cache }

func (f failingL2) Get(context.Context, string) (string, bool, error) {
	return "", false, errors.New("redis down")
}

func TestBalanceGate_RedisErrorFailsOpenOnSnapshot(t *testing.T) {
	g := NewBalanceGate(failingL2{cache.NewMemoryL2()}, nil, quietBalanceLog())
	g.rebase(context.Background(), rows("acct-a", 3.00))
	if ok, rem := g.HasFunds("acct-a"); !ok || rem != 3.00 {
		t.Errorf("redis-down: ok=%v rem=%v, want fail-open on snapshot 3.00", ok, rem)
	}
	// Fail-closed still holds for unknown accounts even with Redis down.
	if ok, _ := g.HasFunds("acct-unknown"); ok {
		t.Errorf("unknown account must stay blocked when redis is down")
	}
}

func TestBalanceGate_DisabledConfigBypasses(t *testing.T) {
	g := NewBalanceGate(cache.NewMemoryL2(), func() bool { return false }, quietBalanceLog())
	// No rebase at all — with the gate disabled everything passes.
	if ok, _ := g.HasFunds("acct-anything"); !ok {
		t.Errorf("disabled gate must not block bidding")
	}
}

func TestBalanceGate_MirrorTTLIsHygieneOnly(t *testing.T) {
	// Documents the contract: the mirror key carries a TTL so Redis doesn't
	// accumulate dead accounts; correctness comes from rebase, not the TTL.
	if balanceMirrorTTL < time.Hour {
		t.Errorf("mirror TTL %v suspiciously short — it must outlive several poll intervals", balanceMirrorTTL)
	}
}
