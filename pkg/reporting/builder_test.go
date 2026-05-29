package reporting

import (
	"context"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

func seedStore(t *testing.T) analytics.Store {
	t.Helper()
	store := analytics.NewMemory()
	ctx := context.Background()
	base := time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)

	// 2 campaigns, 2 geos, 2 days
	for day := 0; day < 2; day++ {
		for _, camp := range []string{"camp-1", "camp-2"} {
			for _, geo := range []string{"GBR", "USA"} {
				for i := 0; i < 5; i++ {
					store.InsertImpression(ctx, &analytics.ImpressionEvent{
						TraceID:          "t",
						CampaignID:       camp,
						CreativeID:       "cr-1",
						PlacementID:      "pl-1",
						PublisherID:      "pub-1",
						AccountID:        "acc-1",
						Geo:              geo,
						Device:           "mobile",
						ClearingPrice:    2.0,
						ClearingCurrency: "USD",
						ClearingPriceUSD: 2.0,
						Timestamp:        base.AddDate(0, 0, day).Add(time.Duration(i) * time.Minute),
					})
				}
			}
		}
	}
	return store
}

func TestBuilder_SimpleQuery(t *testing.T) {
	store := seedStore(t)
	ctx := context.Background()

	result, err := NewBuilder(store).
		Table("impressions").
		Metrics("count", "sum_cost").
		ForAccount("acc-1").
		Build(ctx)

	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(result.Rows))
	}
	count := result.Rows[0][0].(int64)
	if count != 40 { // 2 campaigns * 2 geos * 2 days * 5
		t.Errorf("count = %d, want 40", count)
	}
}

func TestBuilder_GroupBy(t *testing.T) {
	store := seedStore(t)
	ctx := context.Background()

	result, err := NewBuilder(store).
		Table("impressions").
		Metrics("count").
		GroupBy("geo").
		ForAccount("acc-1").
		Build(ctx)

	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 2 {
		t.Fatalf("expected 2 geo groups, got %d", len(result.Rows))
	}
}

func TestBuilder_TimeRange(t *testing.T) {
	store := seedStore(t)
	ctx := context.Background()
	base := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)

	result, err := NewBuilder(store).
		Table("impressions").
		Metrics("count").
		ForAccount("acc-1").
		TimeRange(base, base.Add(24*time.Hour)).
		Build(ctx)

	if err != nil {
		t.Fatal(err)
	}
	count := result.Rows[0][0].(int64)
	if count != 20 { // 2 campaigns * 2 geos * 5 = day 1 only
		t.Errorf("count = %d, want 20", count)
	}
}

func TestBuilder_MissingTable(t *testing.T) {
	store := analytics.NewMemory()
	_, err := NewBuilder(store).Metrics("count").Build(context.Background())
	if err == nil {
		t.Error("expected error for missing table")
	}
}

func TestReport_Execute(t *testing.T) {
	store := seedStore(t)
	ctx := context.Background()
	from := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)
	to := time.Date(2024, 6, 17, 0, 0, 0, 0, time.UTC)

	result, err := CampaignPerformance.Execute(ctx, store, "acc-1", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) == 0 {
		t.Error("expected rows from campaign performance report")
	}
}

func TestReport_GeoBreakdown(t *testing.T) {
	store := seedStore(t)
	ctx := context.Background()
	from := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)
	to := time.Date(2024, 6, 17, 0, 0, 0, 0, time.UTC)

	result, err := GeoBreakdown.Execute(ctx, store, "acc-1", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 2 {
		t.Errorf("expected 2 geos, got %d", len(result.Rows))
	}
}

func TestAllReports(t *testing.T) {
	reports := AllReports()
	if len(reports) != 4 {
		t.Errorf("expected 4 reports, got %d", len(reports))
	}
}
