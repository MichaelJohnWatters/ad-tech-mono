package main

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// transcoderMetrics tracks conditioning outcomes + latency, on the same
// /metrics scrape as the generic HTTP metrics. Cache-hit rate = cached /
// (cached+conditioned); watch conditioned_duration for cold-transcode cost and
// failures for broken creatives/ffmpeg.
type transcoderMetrics struct {
	conditionsTotal *prometheus.CounterVec // result=cached|conditioned|failed
	duration        *prometheus.HistogramVec
}

func newTranscoderMetrics(reg *prometheus.Registry) *transcoderMetrics {
	m := &transcoderMetrics{
		conditionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "adtech",
			Name:      "transcoder_conditions_total",
			Help:      "Ad conditioning requests by outcome. result=cached|conditioned|failed. Cache-hit rate = cached / (cached+conditioned).",
		}, []string{"result"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "adtech",
			Name:      "transcoder_condition_duration_seconds",
			Help:      "Wall-clock per conditioning. A cache hit is sub-ms; a cold transcode is seconds (why serving uses cache_only + async warm).",
			Buckets:   []float64{0.001, 0.01, 0.1, 0.5, 1, 2, 5, 10, 30, 60},
		}, []string{"result"}),
	}
	reg.MustRegister(m.conditionsTotal, m.duration)
	return m
}

func (m *transcoderMetrics) record(result string, since time.Time) {
	if m == nil {
		return
	}
	m.conditionsTotal.WithLabelValues(result).Inc()
	m.duration.WithLabelValues(result).Observe(time.Since(since).Seconds())
}
