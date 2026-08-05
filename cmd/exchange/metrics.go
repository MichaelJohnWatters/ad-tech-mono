package main

import "github.com/prometheus/client_golang/prometheus"

// auctionMetrics groups the exchange's domain-specific Prometheus
// collectors so the dashboards can show fill rate, revenue,
// per-DSP bid behaviour — things the HTTP-level metrics middleware
// can't infer.
//
// All registered on the service-local registry returned by
// middleware.Metrics.Registry() so they're emitted alongside the
// generic adtech_http_* metrics from the same /metrics endpoint.
type auctionMetrics struct {
	// Auctions broken down by outcome — the headline counter the
	// Pipeline Health dashboard's fill-rate panel divides over.
	// Labels:
	//   result = "winner" | "no_bids" | "all_below_floor" | "no_winner"
	//   channel = the auction channel (display, video, all, …)
	auctionsTotal *prometheus.CounterVec

	// Bids RECEIVED by the exchange, broken down per DSP endpoint and
	// whether the DSP returned a bid or not. This is the exchange-side
	// view of bid_rate, complementary to the smart-router JSON debug
	// endpoint. Labels: dsp_endpoint, decision = "bid" | "no_bid".
	bidsReceivedTotal *prometheus.CounterVec

	// Auctions WON per DSP endpoint. Paired with bidsReceivedTotal,
	// this is what the dashboard needs to chart bids vs wins vs no-bids
	// per DSP side by side. The smart router tracks the same thing
	// in-memory via RecordWin; this counter is its Prometheus mirror.
	auctionsWonTotal *prometheus.CounterVec

	// Clearing-price sum — revenue counter. Increments by the cleared
	// price (in USD, not cents) on each winning auction. Use
	// rate()/increase() over time windows for revenue per minute / hour.
	clearingPriceUSDTotal prometheus.Counter

	// Floors-below counter — how often a bid arrived but dropped because
	// it was below the placement floor. Useful for tuning floor prices.
	bidsBelowFloorTotal *prometheus.CounterVec

	// Per-phase auction latency: gates (decode + adstxt/schain/sign) →
	// routing (DSP selection) → fanout (bid collection; the wall-clock
	// dominant phase) → dealeval (deal matching + winner determination) →
	// finalize (win records, response build + encode). Mirrors the Jaeger
	// span boundaries so the dashboard breakdown and traces agree. This is
	// what turns "auction p95 is 900ms" into "fanout is 460 of it" without
	// opening a single trace.
	phaseDuration *prometheus.HistogramVec
}

func newAuctionMetrics(reg *prometheus.Registry) *auctionMetrics {
	m := &auctionMetrics{
		auctionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "adtech",
			Name:      "auctions_total",
			Help:      "Exchange auctions by outcome. result=winner|no_bids|all_below_floor|no_winner.",
		}, []string{"result", "channel"}),
		bidsReceivedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "adtech",
			Name:      "bids_received_total",
			Help:      "Bid responses received by the exchange from each DSP. decision=bid|no_bid.",
		}, []string{"dsp_endpoint", "decision"}),
		clearingPriceUSDTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "adtech",
			Name:      "auction_clearing_price_usd_total",
			Help:      "Sum of clearing prices (USD) across winning auctions. Use rate() for revenue-per-second.",
		}),
		bidsBelowFloorTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "adtech",
			Name:      "bids_below_floor_total",
			Help:      "Bids dropped pre-auction because their price was below the placement floor.",
		}, []string{"placement_id"}),
		phaseDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "adtech",
			Name:      "auction_phase_duration_seconds",
			Help:      "Per-phase auction latency: gates|routing|fanout|dealeval|finalize. Stack the p95s to see where auction time goes.",
			Buckets:   []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5},
		}, []string{"phase"}),
		auctionsWonTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "adtech",
			Name:      "auctions_won_total",
			Help:      "Auctions won per DSP endpoint. Paired with bids_received_total to chart bid/win/loss by DSP.",
		}, []string{"dsp_endpoint"}),
	}
	reg.MustRegister(m.auctionsTotal, m.bidsReceivedTotal, m.clearingPriceUSDTotal, m.bidsBelowFloorTotal, m.auctionsWonTotal, m.phaseDuration)
	return m
}
