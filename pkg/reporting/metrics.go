package reporting

// Derived-metric registry for the QueryEngine. A derived metric is computed
// SERVER-SIDE from one or more base metrics (count, sum_cost) — possibly across
// tables — so clients only ever render, never do business math. See engine.go
// for how these are resolved.

// NetResolver computes a publisher's NET revenue from gross for a window. The
// engine stays decoupled from pkg/billing — cmd/reporting supplies an adapter
// over the warm ContractStore (Contract.CalculateRevenue's PublisherRevenue).
type NetResolver interface {
	// Net returns the publisher's net revenue given gross clearing spend.
	Net(publisherID string, gross float64) float64
}

// metricSource is a (table, base-metric) pair the engine must fetch to compute
// a derived metric. base metrics are the ones the analytics store understands
// directly (count, sum_cost, ...).
type metricSource struct {
	table  string
	metric string
}

// computeCtx is the per-output-row context handed to a derived metric's compute
// func: resolved base values (keyed "table\x00metric"), the row's publisher (for
// net), and the net resolver.
type computeCtx struct {
	vals      map[string]float64
	publisher string
	net       NetResolver
}

func (c computeCtx) get(table, metric string) (float64, bool) {
	v, ok := c.vals[table+"\x00"+metric]
	return v, ok
}

// derivedMetric describes how to compute one derived metric.
//   - sources: the base (table, metric) values it depends on.
//   - needsPublisher: true when the value requires per-publisher context (net).
//   - compute: returns (value, ok); ok=false → emit null (client renders "—").
type derivedMetric struct {
	name           string
	sources        []metricSource
	needsPublisher bool
	compute        func(c computeCtx) (float64, bool)
}

// derivedMetrics is the registry. Ratios are emitted as percentages (matching
// what the portals rendered client-side); eCPM/net are money.
var derivedMetrics = map[string]derivedMetric{
	// eCPM = gross revenue per 1,000 impressions.
	"ecpm": {
		name:    "ecpm",
		sources: []metricSource{{"impressions", "sum_cost"}, {"impressions", "count"}},
		compute: func(c computeCtx) (float64, bool) {
			cost, ok1 := c.get("impressions", "sum_cost")
			cnt, ok2 := c.get("impressions", "count")
			if !ok1 || !ok2 || cnt == 0 {
				return 0, false
			}
			return cost / cnt * 1000, true
		},
	},
	// CTR = clicks / impressions (percent). Cross-table.
	"ctr": {
		name:    "ctr",
		sources: []metricSource{{"clicks", "count"}, {"impressions", "count"}},
		compute: func(c computeCtx) (float64, bool) {
			clk, _ := c.get("clicks", "count") // clicks may be absent for a key → 0
			imp, ok := c.get("impressions", "count")
			if !ok || imp == 0 {
				return 0, false
			}
			return clk / imp * 100, true
		},
	},
	// Raw click count — exposed as a derived metric so it can ride alongside
	// impression-table metrics (count/fill_rate/ecpm/ctr) in ONE grouped query;
	// the engine pulls clicks.count per key cross-table. Absent key → 0 clicks
	// (a real zero, not "—").
	"clicks": {
		name:    "clicks",
		sources: []metricSource{{"clicks", "count"}},
		compute: func(c computeCtx) (float64, bool) {
			clk, _ := c.get("clicks", "count")
			return clk, true
		},
	},
	// Fill rate = filled impressions / ad requests (auctions), percent. Cross-table.
	"fill_rate": {
		name:    "fill_rate",
		sources: []metricSource{{"impressions", "count"}, {"auctions", "count"}},
		compute: func(c computeCtx) (float64, bool) {
			imp, _ := c.get("impressions", "count")
			auc, ok := c.get("auctions", "count")
			if !ok || auc == 0 {
				return 0, false
			}
			return imp / auc * 100, true
		},
	},
	// Viewability rate = IAB-viewable views / impressions (percent). Cross-table.
	// The verdict lives on the views table (iab_viewable, summed); the
	// denominator is served impressions. Filter the report by channel=video to
	// get VIDEO viewability (2s dwell) specifically, or channel=display for
	// display (1s) — the channel filter scopes both sources.
	"viewability_rate": {
		name:    "viewability_rate",
		sources: []metricSource{{"views", "sum_viewable"}, {"impressions", "count"}},
		compute: func(c computeCtx) (float64, bool) {
			viewable, _ := c.get("views", "sum_viewable") // absent for a key → 0
			imp, ok := c.get("impressions", "count")
			if !ok || imp == 0 {
				return 0, false
			}
			return viewable / imp * 100, true
		},
	},
	// Completion rate = completed video/audio plays / started plays (percent).
	// Both sources are filtered counts on media_events (countIf in ClickHouse),
	// so a channel/campaign/etc. filter scopes numerator and denominator alike.
	// Null when nothing started (no plays ≠ 0% completion).
	"completion_rate": {
		name:    "completion_rate",
		sources: []metricSource{{"media_events", "media_completes"}, {"media_events", "media_starts"}},
		compute: func(c computeCtx) (float64, bool) {
			done, _ := c.get("media_events", "media_completes") // absent for a key → 0
			starts, ok := c.get("media_events", "media_starts")
			if !ok || starts == 0 {
				return 0, false
			}
			return done / starts * 100, true
		},
	},
	// Average order value = conversion revenue / conversions. Same-table (conversions).
	"aov": {
		name:    "aov",
		sources: []metricSource{{"conversions", "sum_revenue"}, {"conversions", "count"}},
		compute: func(c computeCtx) (float64, bool) {
			rev, ok1 := c.get("conversions", "sum_revenue")
			cnt, ok2 := c.get("conversions", "count")
			if !ok1 || !ok2 || cnt == 0 {
				return 0, false
			}
			return rev / cnt, true
		},
	},
	// CPA = ad spend / conversions. Cross-table (spend lives on impressions).
	"cpa": {
		name:    "cpa",
		sources: []metricSource{{"impressions", "sum_cost"}, {"conversions", "count"}},
		compute: func(c computeCtx) (float64, bool) {
			cost, _ := c.get("impressions", "sum_cost") // absent key → 0 spend
			cnt, ok := c.get("conversions", "count")
			if !ok || cnt == 0 {
				return 0, false
			}
			return cost / cnt, true
		},
	},
	// Conversion rate = conversions / clicks (percent). Cross-table (post-click).
	"conversion_rate": {
		name:    "conversion_rate",
		sources: []metricSource{{"conversions", "count"}, {"clicks", "count"}},
		compute: func(c computeCtx) (float64, bool) {
			conv, _ := c.get("conversions", "count") // absent key → 0 conversions
			clk, ok := c.get("clicks", "count")
			if !ok || clk == 0 {
				return 0, false
			}
			return conv / clk * 100, true
		},
	},
	// Net revenue = gross × (1 − platform fee), computed via the publisher's
	// contract. Requires publisher scope (see engine).
	"net_revenue": {
		name:           "net_revenue",
		sources:        []metricSource{{"impressions", "sum_cost"}},
		needsPublisher: true,
		compute: func(c computeCtx) (float64, bool) {
			gross, ok := c.get("impressions", "sum_cost")
			if !ok || c.net == nil || c.publisher == "" {
				return 0, false
			}
			return c.net.Net(c.publisher, gross), true
		},
	},
}

// isDerived reports whether a requested metric name is a registered derived metric.
func isDerived(name string) bool {
	_, ok := derivedMetrics[name]
	return ok
}
