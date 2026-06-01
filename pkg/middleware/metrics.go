// HTTP metrics middleware backed by prometheus/client_golang.
//
// Why a real Prometheus client (not the previous hand-rolled stub):
//   - Histograms with proper bucket boundaries give p50/p95/p99 — the
//     stub only had a global average, which is useless for tail latency.
//   - Labels (handler, status_code) let Grafana break down "which
//     endpoint is slow?" without needing to log-scrape Loki.
//   - Counters / gauges deduped + thread-safe out of the box.
//   - promhttp.Handler() emits the canonical OpenMetrics format that
//     Prometheus / Grafana / the OTel ecosystem speak natively.
//
// Same Metrics struct + NewMetrics/Wrap/Handler API as the previous
// stub so callers don't need to change.
package middleware

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics records HTTP request metrics for one service. Each service
// constructs its own (with its service name) at boot; the registered
// collectors are scoped to a fresh *prometheus.Registry to avoid the
// global default registry's process-level metrics being mixed in (we
// add those explicitly via NewGoCollector / NewProcessCollector).
type Metrics struct {
	serviceName string
	registry    *prometheus.Registry

	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	inFlight prometheus.Gauge
	up       prometheus.Gauge
}

// metricsRegistry ensures each (serviceName) only registers once even
// across NewMetrics() calls in tests. Without this, calling NewMetrics
// twice with the same service name panics on duplicate registration.
var metricsRegistry sync.Map // serviceName -> *Metrics

// NewMetrics returns a Metrics for the given service. Repeated calls
// with the same serviceName return the same instance.
func NewMetrics(serviceName string) *Metrics {
	if v, ok := metricsRegistry.Load(serviceName); ok {
		return v.(*Metrics)
	}

	reg := prometheus.NewRegistry()

	// Histogram bucket layout tuned for the ad-tech bid path. The
	// auction-side hot path runs 5-100ms; serving and tracker run
	// 5-50ms; outliers we want visible go up to 5s. Linear buckets
	// would waste resolution at the high end so we go geometric.
	buckets := []float64{
		0.001, 0.002, 0.005, 0.01, 0.02, 0.05, // 1-50ms (hot path)
		0.1, 0.2, 0.5, 1, 2, 5, // 100ms-5s (outliers)
	}

	m := &Metrics{
		serviceName: serviceName,
		registry:    reg,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "adtech",
			Subsystem: "http",
			Name:      "requests_total",
			Help:      "Total HTTP requests by handler + status code.",
		}, []string{"service", "handler", "method", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "adtech",
			Subsystem: "http",
			Name:      "request_duration_seconds",
			Help:      "HTTP request latency by handler + status code. Bucket layout favours sub-100ms resolution for the bid hot path.",
			Buckets:   buckets,
		}, []string{"service", "handler", "method", "status"}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace:   "adtech",
			Subsystem:   "http",
			Name:        "in_flight_requests",
			Help:        "Number of HTTP requests currently being handled.",
			ConstLabels: prometheus.Labels{"service": serviceName},
		}),
		up: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace:   "adtech",
			Name:        "up",
			Help:        "1 if the service is up.",
			ConstLabels: prometheus.Labels{"service": serviceName},
		}),
	}
	reg.MustRegister(m.requests, m.duration, m.inFlight, m.up)
	// Process + Go runtime metrics (memory, goroutines, GC, fd count)
	// — invaluable for spotting "service slow because GC is thrashing"
	// or "goroutine leak in the bid path."
	reg.MustRegister(prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{Namespace: "adtech"}))
	reg.MustRegister(prometheus.NewGoCollector())
	m.up.Set(1)

	metricsRegistry.Store(serviceName, m)
	return m
}

// Wrap returns middleware that records request count + duration +
// in-flight gauge per handler. The handler label is the request URL
// path with any high-cardinality tail trimmed — see normalizePath.
func (m *Metrics) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		m.inFlight.Inc()
		defer m.inFlight.Dec()

		rw := &responseWriter{ResponseWriter: w, statusCode: 200}
		next.ServeHTTP(rw, r)

		handler := normalizePath(r.URL.Path)
		status := strconv.Itoa(rw.statusCode)
		labels := prometheus.Labels{
			"service": m.serviceName,
			"handler": handler,
			"method":  r.Method,
			"status":  status,
		}
		m.requests.With(labels).Inc()
		m.duration.With(labels).Observe(time.Since(start).Seconds())
	})
}

// Handler returns the /metrics HTTP handler that emits the OpenMetrics
// text format Prometheus scrapes. promhttp.HandlerFor binds it to this
// service's local registry (not the default global one).
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	})
}

// Registry exposes the service-local prometheus registry so services can
// register additional domain-specific collectors (e.g. exchange registers
// adtech_auctions_total{result=...} for the Pipeline Health dashboard).
// Anything registered here lands in the same /metrics scrape so there's
// no separate endpoint to wire.
func (m *Metrics) Registry() *prometheus.Registry {
	return m.registry
}

// normalizePath strips high-cardinality path segments (UUIDs, IDs) so
// the handler label stays bounded. Without this, "GET /v1/dsp/campaigns/abc-123"
// and "GET /v1/dsp/campaigns/def-456" would explode the cardinality of
// the metrics — Prometheus best practice is bounded label sets.
//
// Heuristic: any segment that looks like a UUID or a long hex/digit
// string gets replaced with ":id". Tunable later if we want stricter
// or laxer matching.
func normalizePath(p string) string {
	if p == "" {
		return "/"
	}
	out := make([]byte, 0, len(p))
	for i := 0; i < len(p); {
		// Find segment boundaries.
		if p[i] != '/' {
			i++
			continue
		}
		out = append(out, '/')
		j := i + 1
		for j < len(p) && p[j] != '/' {
			j++
		}
		seg := p[i+1 : j]
		if looksLikeID(seg) {
			out = append(out, []byte(":id")...)
		} else {
			out = append(out, []byte(seg)...)
		}
		i = j
	}
	if len(out) == 0 {
		return "/"
	}
	return string(out)
}

// looksLikeID is true if the segment is plausibly a UUID, hex hash, or
// long numeric ID — anything we want collapsed to ":id" to bound cardinality.
func looksLikeID(s string) bool {
	if len(s) >= 32 {
		return true // long hash / UUID
	}
	if len(s) == 36 && s[8] == '-' && s[13] == '-' && s[18] == '-' && s[23] == '-' {
		return true // canonical UUID
	}
	// All-digit segment ≥4 chars (numeric IDs).
	if len(s) >= 4 {
		allDigit := true
		for i := 0; i < len(s); i++ {
			if s[i] < '0' || s[i] > '9' {
				allDigit = false
				break
			}
		}
		if allDigit {
			return true
		}
	}
	return false
}

type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}
