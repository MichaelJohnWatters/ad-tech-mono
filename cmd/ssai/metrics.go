package main

import (
	"github.com/prometheus/client_golang/prometheus"
)

// stitcherMetrics is the SSAI serving-path telemetry, on the same /metrics scrape
// as the generic HTTP metrics. This is the dashboard you actually watch: how many
// avails fill, how often the serve-time conditioning cache hits, and how deep the
// ad pods run.
type stitcherMetrics struct {
	breaks    *prometheus.CounterVec // result=filled|slate|unfilled|error
	condCache *prometheus.CounterVec // result=hit|miss (serve-time cache_only lookups)
	adSeconds prometheus.Counter     // total ad seconds stitched
	podAds    prometheus.Histogram   // ads per filled break (pod depth)
}

func newStitcherMetrics(reg *prometheus.Registry) *stitcherMetrics {
	m := &stitcherMetrics{
		breaks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "adtech",
			Name:      "ssai_breaks_total",
			Help:      "Ad avails by outcome. result=filled (>=1 ad), slate (slate spliced), unfilled (kept content), error (winner couldn't be conditioned). Fill rate = filled / sum.",
		}, []string{"result"}),
		condCache: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "adtech",
			Name:      "ssai_condition_cache_total",
			Help:      "Serve-time (cache_only) conditioning lookups. result=hit|miss. A miss means the ad wasn't pre-conditioned and the break slated/kept content while warming.",
		}, []string{"result"}),
		adSeconds: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "adtech",
			Name:      "ssai_ad_seconds_total",
			Help:      "Total seconds of ads stitched into content across all requests.",
		}),
		podAds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: "adtech",
			Name:      "ssai_pod_ads",
			Help:      "Number of ads stitched into a single filled avail (ad-pod depth).",
			Buckets:   []float64{1, 2, 3, 4, 5, 6, 8},
		}),
	}
	reg.MustRegister(m.breaks, m.condCache, m.adSeconds, m.podAds)
	return m
}

func (m *stitcherMetrics) recordBreak(result string) {
	if m == nil {
		return
	}
	m.breaks.WithLabelValues(result).Inc()
}

func (m *stitcherMetrics) recordCond(hit bool) {
	if m == nil {
		return
	}
	result := "miss"
	if hit {
		result = "hit"
	}
	m.condCache.WithLabelValues(result).Inc()
}

func (m *stitcherMetrics) recordPod(ads int, adSeconds float64) {
	if m == nil {
		return
	}
	m.podAds.Observe(float64(ads))
	m.adSeconds.Add(adSeconds)
}
