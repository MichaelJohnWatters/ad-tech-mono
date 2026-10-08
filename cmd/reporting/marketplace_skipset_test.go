package main

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache/warm"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

type fakeBuyerLoader struct{ buyers []string }

func (f *fakeBuyerLoader) LoadAll(context.Context) ([]string, error) { return f.buyers, nil }
func (f *fakeBuyerLoader) KeyOf(id string) string                    { return id }

func testBuyerCache(t *testing.T, buyers []string) *warm.Cache[string] {
	t.Helper()
	c := warm.New(warm.Config[string]{
		Name:         "marketplace_grant_buyers",
		Loader:       &fakeBuyerLoader{buyers: buyers},
		Clock:        clock.Real{},
		PollInterval: time.Hour,
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("cache start: %v", err)
	}
	t.Cleanup(c.Stop)
	return c
}

// TestMarketplaceAccrualSkipSet: a grant-less buyer must return BEFORE any
// Postgres work. The accrual gets a *sql.DB whose first use would fail
// (unreachable address) — the skip path never touches it, so no error logs;
// an in-set buyer proceeds to BeginTx and hits the connection error, which
// proves the gate admits exactly the cached buyers. (handoff 08: the skipped
// probe TX was ~28% of PG time at 150rps.)
func TestMarketplaceAccrualSkipSet(t *testing.T) {
	db, err := sql.Open("postgres", "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	var logged []string
	log := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		logged = append(logged, string(p))
		return len(p), nil
	}), nil))

	a := newMarketplaceAccrual(db, func() float64 { return 10 },
		testBuyerCache(t, []string{"buyer-with-grant"}), log)
	imp := func(account string) *analytics.ImpressionEvent {
		return &analytics.ImpressionEvent{TraceID: "t1", AccountID: account, CampaignID: "c1"}
	}

	// Grant-less buyer: skip set short-circuits — no DB touch, no error logged.
	a.AccrueOnImpression(context.Background(), imp("buyer-without-grant"))
	if len(logged) != 0 {
		t.Fatalf("grant-less buyer reached the DB path: %v", logged)
	}

	// In-set buyer: passes the gate, reaches BeginTx, fails on the dead DB —
	// the ERROR log is the proof the gate let it through.
	a.AccrueOnImpression(context.Background(), imp("buyer-with-grant"))
	if len(logged) == 0 {
		t.Fatal("in-set buyer never reached the DB path (gate too strict — would miss surcharges)")
	}

	// nil cache (initial load failed at boot): fail OPEN — everyone takes the
	// full TX path, nobody is skipped.
	logged = nil
	noCache := newMarketplaceAccrual(db, func() float64 { return 10 }, nil, log)
	noCache.AccrueOnImpression(context.Background(), imp("buyer-without-grant"))
	if len(logged) == 0 {
		t.Fatal("nil skip set must fail open to the TX path, not skip")
	}
}

type writerFunc func(p []byte) (int, error)

func (w writerFunc) Write(p []byte) (int, error) { return w(p) }
