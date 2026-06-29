package analytics

import (
	"context"
	"testing"
	"time"
)

func TestIsIABViewable(t *testing.T) {
	cases := []struct {
		name           string
		durMs          int64
		pct            int
		areaPx         int64
		wantIABViewable bool
	}{
		{"below time threshold", 999, 100, 0, false},
		{"at time threshold, 50% visible", 1000, 50, 0, true},
		{"at time threshold, 49% visible", 1000, 49, 0, false},
		{"large ad, 30% suffices", 1500, 30, 250_000, true},
		{"large ad, 29% fails", 1500, 29, 250_000, false},
		{"large ad below time threshold still fails", 500, 100, 250_000, false},
		{"zero values fail", 0, 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := IsIABViewable(c.durMs, c.pct, c.areaPx)
			if got != c.wantIABViewable {
				t.Errorf("IsIABViewable(%dms, %d%%, %dpx) = %v, want %v", c.durMs, c.pct, c.areaPx, got, c.wantIABViewable)
			}
		})
	}
}

func TestMemoryStore_InsertView(t *testing.T) {
	store := NewMemory()
	ctx := context.Background()
	now := time.Now().UTC()

	if err := store.InsertView(ctx, &ViewEvent{
		TraceID:        "tr-1",
		CampaignID:     "camp-1",
		CreativeID:     "cr-1",
		PlacementID:    "pl-1",
		PublisherID:    "pub-1",
		AccountID:      "acc-1",
		DurationMs:     2000,
		PercentVisible: 75,
		AreaPx:         90000,
		IABViewable:    true,
		Timestamp:      now,
	}); err != nil {
		t.Fatal(err)
	}

	if got := store.Views(); len(got) != 1 || !got[0].IABViewable || got[0].DurationMs != 2000 {
		t.Fatalf("Views() = %+v", got)
	}

	res, err := store.Query(ctx, QueryParams{Table: "views"})
	if err != nil {
		t.Fatal(err)
	}
	// First column is "count", second is "viewable_count"
	if res.Rows[0][0].(int64) != 1 || res.Rows[0][1].(int64) != 1 {
		t.Errorf("query result: %+v, want count=1 viewable_count=1", res.Rows[0])
	}
}

func TestMemoryStore_InsertAndQuery(t *testing.T) {
	store := NewMemory()
	ctx := context.Background()
	now := time.Date(2024, 6, 15, 10, 30, 0, 0, time.UTC)

	// Insert impressions
	for i := 0; i < 5; i++ {
		err := store.InsertImpression(ctx, &ImpressionEvent{
			TraceID:          "trace-1",
			CampaignID:       "camp-1",
			CreativeID:       "cr-1",
			PlacementID:      "pl-1",
			PublisherID:      "pub-1",
			AccountID:        "acc-1",
			Geo:              "GBR",
			Device:           "mobile",
			Channel:          "display",
			ClearingPrice:    2.50,
			ClearingCurrency: "USD",
			ClearingPriceUSD: 2.50,
			SchemaVersion:    1,
			Timestamp:        now,
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// Insert for a different campaign
	err := store.InsertImpression(ctx, &ImpressionEvent{
		TraceID:          "trace-2",
		CampaignID:       "camp-2",
		CreativeID:       "cr-2",
		PlacementID:      "pl-1",
		PublisherID:      "pub-1",
		AccountID:        "acc-1",
		Geo:              "USA",
		Device:           "desktop",
		Channel:          "display",
		ClearingPrice:    3.00,
		ClearingCurrency: "USD",
		ClearingPriceUSD: 3.00,
		SchemaVersion:    1,
		Timestamp:        now,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Count check
	imps, clicks, convs, aucts := store.Counts()
	if imps != 6 {
		t.Errorf("impressions count = %d, want 6", imps)
	}
	if clicks != 0 || convs != 0 || aucts != 0 {
		t.Error("expected zero clicks/conversions/auctions")
	}

	// Query all impressions
	result, err := store.Query(ctx, QueryParams{
		Table:   "impressions",
		Metrics: []string{"count", "sum_cost"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(result.Rows))
	}
	if result.Rows[0][0].(int64) != 6 {
		t.Errorf("count = %v, want 6", result.Rows[0][0])
	}

	// Query with campaign filter
	result, err = store.Query(ctx, QueryParams{
		Table:   "impressions",
		Metrics: []string{"count"},
		Filters: map[string]string{"campaign_id": "camp-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Rows[0][0].(int64) != 5 {
		t.Errorf("filtered count = %v, want 5", result.Rows[0][0])
	}

	// Query grouped by campaign_id
	result, err = store.Query(ctx, QueryParams{
		Table:      "impressions",
		Metrics:    []string{"count", "sum_cost"},
		Dimensions: []string{"campaign_id"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(result.Rows))
	}
}

func TestMemoryStore_Clicks(t *testing.T) {
	store := NewMemory()
	ctx := context.Background()
	now := time.Now()

	store.InsertClick(ctx, &ClickEvent{
		TraceID:     "trace-1",
		CampaignID:  "camp-1",
		CreativeID:  "cr-1",
		PlacementID: "pl-1",
		PublisherID: "pub-1",
		AccountID:   "acc-1",
		LandingURL:  "https://example.com",
		Timestamp:   now,
	})

	result, err := store.Query(ctx, QueryParams{Table: "clicks"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Rows[0][0].(int64) != 1 {
		t.Errorf("click count = %v, want 1", result.Rows[0][0])
	}
}

func TestMemoryStore_Conversions(t *testing.T) {
	store := NewMemory()
	ctx := context.Background()
	now := time.Now()

	store.InsertConversion(ctx, &ConversionEvent{
		TraceID:        "trace-1",
		CampaignID:     "camp-1",
		CreativeID:     "cr-1",
		PlacementID:    "pl-1",
		AccountID:      "acc-1",
		ConversionType: "purchase",
		Revenue:        50.00,
		Currency:       "USD",
		RevenueUSD:     50.00,
		Timestamp:      now,
	})

	result, err := store.Query(ctx, QueryParams{Table: "conversions"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Rows[0][0].(int64) != 1 {
		t.Errorf("conversion count = %v, want 1", result.Rows[0][0])
	}
	if result.Rows[0][1].(float64) != 50.00 {
		t.Errorf("sum_revenue = %v, want 50", result.Rows[0][1])
	}
}

func TestMemoryStore_Auctions(t *testing.T) {
	store := NewMemory()
	ctx := context.Background()
	now := time.Now()

	store.InsertAuction(ctx, &AuctionEvent{
		TraceID:       "trace-1",
		PlacementID:   "pl-1",
		PublisherID:   "pub-1",
		Channel:       "display",
		NumBids:       3,
		WinningBid:    5.00,
		ClearingPrice: 4.50,
		DurationMs:    45,
		Timestamp:     now,
	})
	store.InsertAuction(ctx, &AuctionEvent{
		TraceID:       "trace-2",
		PlacementID:   "pl-1",
		PublisherID:   "pub-1",
		Channel:       "display",
		NumBids:       2,
		WinningBid:    3.00,
		ClearingPrice: 2.80,
		DurationMs:    55,
		Timestamp:     now,
	})

	result, err := store.Query(ctx, QueryParams{Table: "auctions"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Rows[0][0].(int64) != 2 {
		t.Errorf("auction count = %v, want 2", result.Rows[0][0])
	}
	if result.Rows[0][1].(float64) != 50.0 {
		t.Errorf("avg_duration_ms = %v, want 50", result.Rows[0][1])
	}
}

func TestMemoryStore_BatchInsert(t *testing.T) {
	store := NewMemory()
	ctx := context.Background()
	now := time.Now()

	err := store.InsertBatch(ctx, []Event{
		{Type: EventImpression, Impression: &ImpressionEvent{
			TraceID: "t1", CampaignID: "c1", CreativeID: "cr1",
			PlacementID: "p1", PublisherID: "pub1", AccountID: "a1",
			ClearingPrice: 1.0, ClearingCurrency: "USD", ClearingPriceUSD: 1.0,
			Timestamp: now,
		}},
		{Type: EventClick, Click: &ClickEvent{
			TraceID: "t1", CampaignID: "c1", CreativeID: "cr1",
			PlacementID: "p1", PublisherID: "pub1", AccountID: "a1",
			Timestamp: now,
		}},
		{Type: EventConversion, Conversion: &ConversionEvent{
			TraceID: "t1", CampaignID: "c1", CreativeID: "cr1",
			PlacementID: "p1", AccountID: "a1", ConversionType: "signup",
			Timestamp: now,
		}},
		{Type: EventAuction, Auction: &AuctionEvent{
			TraceID: "t1", PlacementID: "p1", PublisherID: "pub1",
			NumBids: 3, DurationMs: 30, Timestamp: now,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	imps, clicks, convs, aucts := store.Counts()
	if imps != 1 || clicks != 1 || convs != 1 || aucts != 1 {
		t.Errorf("counts = %d/%d/%d/%d, want 1/1/1/1", imps, clicks, convs, aucts)
	}
}

func TestMemoryStore_TimeRangeFilter(t *testing.T) {
	store := NewMemory()
	ctx := context.Background()

	base := time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		store.InsertImpression(ctx, &ImpressionEvent{
			TraceID: "t", CampaignID: "c", CreativeID: "cr",
			PlacementID: "p", PublisherID: "pub", AccountID: "a",
			ClearingPrice: 1.0, ClearingCurrency: "USD", ClearingPriceUSD: 1.0,
			Timestamp: base.Add(time.Duration(i) * time.Hour),
		})
	}

	// Query first 5 hours only
	result, err := store.Query(ctx, QueryParams{
		Table:    "impressions",
		Metrics:  []string{"count"},
		TimeFrom: base,
		TimeTo:   base.Add(4 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Rows[0][0].(int64) != 5 {
		t.Errorf("time-filtered count = %v, want 5", result.Rows[0][0])
	}
}

func TestMemoryStore_UnknownTable(t *testing.T) {
	store := NewMemory()
	_, err := store.Query(context.Background(), QueryParams{Table: "nonexistent"})
	if err == nil {
		t.Error("expected error for unknown table")
	}
}

func TestStoreInterface(t *testing.T) {
	// Compile-time check that MemoryStore implements Store
	var _ Store = (*MemoryStore)(nil)
}

func TestBuildQuery(t *testing.T) {
	tests := []struct {
		name   string
		params QueryParams
		want   string
	}{
		{
			name: "simple count",
			params: QueryParams{
				Table:   "impressions",
				Metrics: []string{"count"},
			},
			want: "SELECT COUNT(*) AS count FROM impressions",
		},
		{
			name: "grouped with filter",
			params: QueryParams{
				Table:      "impressions",
				Metrics:    []string{"count", "sum_cost"},
				Dimensions: []string{"campaign_id"},
				Filters:    map[string]string{"geo": "GBR"},
			},
			want: "SELECT campaign_id, COUNT(*) AS count, SUM(clearing_price_usd) AS sum_cost FROM impressions WHERE geo = ? GROUP BY campaign_id",
		},
		{
			name: "day dimension",
			params: QueryParams{
				Table:      "impressions",
				Metrics:    []string{"count"},
				Dimensions: []string{"day"},
				OrderBy:    "day",
				OrderDir:   "desc",
				Limit:      7,
			},
			want: "SELECT CAST(timestamp AS DATE) AS day, COUNT(*) AS count FROM impressions GROUP BY CAST(timestamp AS DATE) ORDER BY day DESC LIMIT 7",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := BuildQuery(tt.params)
			if got != tt.want {
				t.Errorf("\ngot:  %s\nwant: %s", got, tt.want)
			}
		})
	}
}
