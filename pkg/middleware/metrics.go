package middleware

import (
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Metrics collects HTTP request metrics for Prometheus scraping.
type Metrics struct {
	mu              sync.RWMutex
	requestCount    map[string]*int64 // method:path -> count
	errorCount      map[string]*int64 // status_code -> count
	totalRequests   int64
	totalErrors     int64
	totalLatencyMs  int64
	serviceName     string
}

// NewMetrics creates a metrics collector for a service.
func NewMetrics(serviceName string) *Metrics {
	return &Metrics{
		requestCount: make(map[string]*int64),
		errorCount:   make(map[string]*int64),
		serviceName:  serviceName,
	}
}

// Wrap returns middleware that records request metrics.
func (m *Metrics) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Wrap response writer to capture status code
		rw := &responseWriter{ResponseWriter: w, statusCode: 200}
		next.ServeHTTP(rw, r)

		latency := time.Since(start).Milliseconds()
		atomic.AddInt64(&m.totalRequests, 1)
		atomic.AddInt64(&m.totalLatencyMs, latency)

		if rw.statusCode >= 400 {
			atomic.AddInt64(&m.totalErrors, 1)
			key := fmt.Sprintf("%d", rw.statusCode)
			m.mu.Lock()
			if m.errorCount[key] == nil {
				v := int64(0)
				m.errorCount[key] = &v
			}
			atomic.AddInt64(m.errorCount[key], 1)
			m.mu.Unlock()
		}

		key := r.Method + ":" + r.URL.Path
		m.mu.Lock()
		if m.requestCount[key] == nil {
			v := int64(0)
			m.requestCount[key] = &v
		}
		atomic.AddInt64(m.requestCount[key], 1)
		m.mu.Unlock()
	})
}

// Handler returns an HTTP handler that serves Prometheus-format metrics.
func (m *Metrics) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")

		total := atomic.LoadInt64(&m.totalRequests)
		errors := atomic.LoadInt64(&m.totalErrors)
		latency := atomic.LoadInt64(&m.totalLatencyMs)

		fmt.Fprintf(w, "# HELP adtech_http_requests_total Total HTTP requests\n")
		fmt.Fprintf(w, "# TYPE adtech_http_requests_total counter\n")
		fmt.Fprintf(w, "adtech_http_requests_total{service=\"%s\"} %d\n", m.serviceName, total)

		fmt.Fprintf(w, "# HELP adtech_http_errors_total Total HTTP errors (4xx/5xx)\n")
		fmt.Fprintf(w, "# TYPE adtech_http_errors_total counter\n")
		fmt.Fprintf(w, "adtech_http_errors_total{service=\"%s\"} %d\n", m.serviceName, errors)

		m.mu.RLock()
		for code, count := range m.errorCount {
			fmt.Fprintf(w, "adtech_http_errors_total{service=\"%s\",status_code=\"%s\"} %d\n", m.serviceName, code, atomic.LoadInt64(count))
		}
		m.mu.RUnlock()

		var avgLatency float64
		if total > 0 {
			avgLatency = float64(latency) / float64(total)
		}
		fmt.Fprintf(w, "# HELP adtech_http_request_duration_ms Average request duration\n")
		fmt.Fprintf(w, "# TYPE adtech_http_request_duration_ms gauge\n")
		fmt.Fprintf(w, "adtech_http_request_duration_ms{service=\"%s\"} %.2f\n", m.serviceName, avgLatency)

		fmt.Fprintf(w, "# HELP adtech_up Service is up\n")
		fmt.Fprintf(w, "# TYPE adtech_up gauge\n")
		fmt.Fprintf(w, "adtech_up{service=\"%s\"} 1\n", m.serviceName)
	}
}

type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}
