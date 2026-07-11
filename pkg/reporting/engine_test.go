package reporting

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

// fakeNet is a flat-fee NetResolver for tests.
type fakeNet struct{ feePct float64 }

func (f fakeNet) Net(_ string, gross float64) float64 { return gross * (1 - f.feePct/100) }

// seed builds a memory store with a known world: 100 impressions @ $0.003 for
// pubA/pl1, 125 auctions, 5 clicks — plus 40 impressions for pubB/pl2 (the
// other tenant, to catch isolation leaks).
func seed(t *testing.T) *analytics.MemoryStore {
	t.Helper()
	s := analytics.NewMemory()
	ctx := context.Background()
	ts := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 100; i++ {
		_ = s.InsertImpression(ctx, &analytics.ImpressionEvent{PublisherID: "pubA", PlacementID: "pl1", ClearingPriceUSD: 0.003, Timestamp: ts})
	}
	for i := 0; i < 125; i++ {
		_ = s.InsertAuction(ctx, &analytics.AuctionEvent{PublisherID: "pubA", PlacementID: "pl1", Timestamp: ts})
	}
	for i := 0; i < 5; i++ {
		_ = s.InsertClick(ctx, &analytics.ClickEvent{PublisherID: "pubA", PlacementID: "pl1", Timestamp: ts})
	}
	for i := 0; i < 40; i++ {
		_ = s.InsertImpression(ctx, &analytics.ImpressionEvent{PublisherID: "pubB", PlacementID: "pl2", ClearingPriceUSD: 0.010, Timestamp: ts})
	}
	return s
}

func col(t *testing.T, res *analytics.QueryResult, name string) float64 {
	t.Helper()
	if len(res.Rows) == 0 {
		t.Fatalf("no rows in result %+v", res)
	}
	for i, c := range res.Columns {
		if c == name {
			f, ok := toFloat(res.Rows[0][i])
			if !ok {
				t.Fatalf("column %q not numeric: %v", name, res.Rows[0][i])
			}
			return f
		}
	}
	t.Fatalf("column %q not in %v", name, res.Columns)
	return 0
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestEngine_PassThrough_Identical(t *testing.T) {
	s := seed(t)
	eng := NewQueryEngine(s, nil)
	p := analytics.QueryParams{Table: "impressions", Metrics: []string{"count", "sum_cost"}, Filters: map[string]string{"publisher_id": "pubA"}}

	raw, _ := s.Query(context.Background(), p)
	got, err := eng.Query(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if col(t, got, "count") != col(t, raw, "count") || col(t, got, "sum_cost") != col(t, raw, "sum_cost") {
		t.Errorf("pass-through diverged: raw=%v engine=%v", raw.Rows, got.Rows)
	}
}

func TestEngine_Ecpm(t *testing.T) {
	eng := NewQueryEngine(seed(t), nil)
	res, err := eng.Query(context.Background(), analytics.QueryParams{
		Table: "impressions", Metrics: []string{"ecpm"}, Filters: map[string]string{"publisher_id": "pubA"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 100 imps × $0.003 = $0.30 gross → eCPM = 0.30/100*1000 = $3.00
	if got := col(t, res, "ecpm"); !approx(got, 3.0) {
		t.Errorf("ecpm = %v, want 3.0", got)
	}
}

func TestEngine_FillRate_CrossTable(t *testing.T) {
	eng := NewQueryEngine(seed(t), nil)
	res, err := eng.Query(context.Background(), analytics.QueryParams{
		Table: "impressions", Metrics: []string{"fill_rate"}, Filters: map[string]string{"publisher_id": "pubA"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 100 impressions / 125 auctions = 80%
	if got := col(t, res, "fill_rate"); !approx(got, 80.0) {
		t.Errorf("fill_rate = %v, want 80.0", got)
	}
}

func TestEngine_Ctr_CrossTable(t *testing.T) {
	eng := NewQueryEngine(seed(t), nil)
	res, err := eng.Query(context.Background(), analytics.QueryParams{
		Table: "impressions", Metrics: []string{"ctr"}, Filters: map[string]string{"publisher_id": "pubA"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 5 clicks / 100 impressions = 5%
	if got := col(t, res, "ctr"); !approx(got, 5.0) {
		t.Errorf("ctr = %v, want 5.0", got)
	}
}

func TestEngine_NetRevenue_ContractAware(t *testing.T) {
	eng := NewQueryEngine(seed(t), fakeNet{feePct: 25})
	res, err := eng.Query(context.Background(), analytics.QueryParams{
		Table: "impressions", Metrics: []string{"sum_cost", "net_revenue"}, Filters: map[string]string{"publisher_id": "pubA"},
	})
	if err != nil {
		t.Fatal(err)
	}
	gross := col(t, res, "sum_cost")
	net := col(t, res, "net_revenue")
	if !approx(gross, 0.30) {
		t.Errorf("gross = %v, want 0.30", gross)
	}
	if !approx(net, 0.30*0.75) {
		t.Errorf("net = %v, want %v (gross × 0.75)", net, 0.30*0.75)
	}
}

func TestEngine_NetRevenue_RequiresPublisherScope(t *testing.T) {
	eng := NewQueryEngine(seed(t), fakeNet{feePct: 25})
	// No publisher_id filter and not grouped by publisher_id → must error.
	_, err := eng.Query(context.Background(), analytics.QueryParams{
		Table: "impressions", Metrics: []string{"net_revenue"},
	})
	if err == nil {
		t.Fatal("expected error: net_revenue without publisher scope")
	}
}

// TestEngine_TenantIsolation: a scoped query must never return another tenant's
// rows, and every cross-table sub-query must carry the same filter (else the
// auctions/clicks sub-query would leak the other publisher's counts).
func TestEngine_TenantIsolation(t *testing.T) {
	eng := NewQueryEngine(seed(t), fakeNet{feePct: 20})
	// pubA has 100 imps; pubB has 40. Scoped to pubA, count must be exactly 100
	// (not 140), and a derived query must not pull pubB data through any table.
	res, err := eng.Query(context.Background(), analytics.QueryParams{
		Table:   "impressions",
		Metrics: []string{"count", "ecpm", "net_revenue"},
		Filters: map[string]string{"publisher_id": "pubA"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := col(t, res, "count"); !approx(got, 100) {
		t.Errorf("scoped count = %v, want 100 (pubB rows leaked?)", got)
	}
	// eCPM uses pubA cost only (0.003), not pubB's 0.010.
	if got := col(t, res, "ecpm"); !approx(got, 3.0) {
		t.Errorf("scoped ecpm = %v, want 3.0 (pubB cost leaked?)", got)
	}
}
