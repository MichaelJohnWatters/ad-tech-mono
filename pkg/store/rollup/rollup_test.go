package rollup

import (
	"context"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

func TestEngine_RunLevel(t *testing.T) {
	store := analytics.NewMemory()
	ctx := context.Background()

	// Insert test data in the 13:00-14:00 hour
	base := time.Date(2024, 6, 15, 13, 0, 0, 0, time.UTC)
	campaigns := []string{"camp-1", "camp-2"}
	for i := 0; i < 60; i++ {
		for _, cid := range campaigns {
			store.InsertImpression(ctx, &analytics.ImpressionEvent{
				TraceID: "t", CampaignID: cid, CreativeID: "cr",
				PlacementID: "pl", PublisherID: "pub", AccountID: "acc",
				Geo: "GBR", Device: "mobile",
				ClearingPrice: 2.0, ClearingCurrency: "USD", ClearingPriceUSD: 2.0,
				Timestamp: base.Add(time.Duration(i) * time.Minute),
			})
		}
	}

	// Set clock to 14:30 so the hourly window covers 13:00-14:00
	clk := clock.NewFake(time.Date(2024, 6, 15, 14, 30, 0, 0, time.UTC))
	log := logger.New("rollup-test")

	engine := NewEngine(store, clk, log)
	engine.Register(EventsConfig)

	results, err := engine.RunLevel(ctx, Hourly)
	if err != nil {
		t.Fatal(err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r := results[0]
	if r.Config != "events" {
		t.Errorf("config = %s, want events", r.Config)
	}
	if r.Level != Hourly {
		t.Errorf("level = %s, want hourly", r.Level)
	}
	if r.RowsRead == 0 {
		t.Error("expected rows to be read")
	}
}

func TestWindowForLevel(t *testing.T) {
	now := time.Date(2024, 6, 15, 14, 35, 22, 0, time.UTC)

	tests := []struct {
		level    Level
		wantFrom time.Time
		wantTo   time.Time
	}{
		{Minute, time.Date(2024, 6, 15, 14, 34, 0, 0, time.UTC), time.Date(2024, 6, 15, 14, 35, 0, 0, time.UTC)},
		{Hourly, time.Date(2024, 6, 15, 13, 0, 0, 0, time.UTC), time.Date(2024, 6, 15, 14, 0, 0, 0, time.UTC)},
		{Daily, time.Date(2024, 6, 14, 0, 0, 0, 0, time.UTC), time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)},
		{Monthly, time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)},
	}

	for _, tt := range tests {
		t.Run(string(tt.level), func(t *testing.T) {
			from, to := windowForLevel(tt.level, now)
			if !from.Equal(tt.wantFrom) {
				t.Errorf("from = %v, want %v", from, tt.wantFrom)
			}
			if !to.Equal(tt.wantTo) {
				t.Errorf("to = %v, want %v", to, tt.wantTo)
			}
		})
	}
}

func TestTierForRange(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name string
		from time.Time
		to   time.Time
		want Level
	}{
		{"15min", now.Add(-15 * time.Minute), now, Minute},
		{"6h", now.Add(-6 * time.Hour), now, Hourly},
		{"7d", now.Add(-7 * 24 * time.Hour), now, Daily},
		{"1y", now.Add(-365 * 24 * time.Hour), now, Monthly},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TierForRange(tt.from, tt.to)
			if got != tt.want {
				t.Errorf("tier = %s, want %s", got, tt.want)
			}
		})
	}
}
