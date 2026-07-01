package rollup

import (
	"context"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

func TestEngine_RunLevelIdempotent(t *testing.T) {
	store := analytics.NewMemory()
	ctx := context.Background()
	base := time.Date(2024, 6, 15, 13, 0, 0, 0, time.UTC)
	for i := 0; i < 60; i++ {
		for _, cid := range []string{"camp-1", "camp-2"} {
			store.InsertImpression(ctx, &analytics.ImpressionEvent{
				TraceID: "t", CampaignID: cid, CreativeID: "cr", PlacementID: "pl",
				PublisherID: "pub", AccountID: "acc", Geo: "GBR", Device: "mobile",
				ClearingPrice: 2.0, ClearingCurrency: "USD", ClearingPriceUSD: 2.0,
				Timestamp: base.Add(time.Duration(i) * time.Minute),
			})
		}
	}
	clk := clock.NewFake(time.Date(2024, 6, 15, 14, 30, 0, 0, time.UTC))
	e := NewEngine(store, clk, logger.New("rollup-test"))
	e.Register(EventsConfig)

	if _, err := e.RunLevel(ctx, Hourly); err != nil {
		t.Fatal(err)
	}
	first := store.RollupCount("events", "hourly")
	if first == 0 {
		t.Fatal("expected rollup rows after first run")
	}
	// Second run over the same window must not duplicate.
	if _, err := e.RunLevel(ctx, Hourly); err != nil {
		t.Fatal(err)
	}
	if second := store.RollupCount("events", "hourly"); second != first {
		t.Fatalf("re-run rollup count = %d, want %d (idempotent)", second, first)
	}
}
