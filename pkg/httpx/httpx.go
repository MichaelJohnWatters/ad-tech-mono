// Package httpx holds process-wide HTTP client tuning shared by every service.
package httpx

import (
	"net/http"
	"sync"
	"time"
)

// Pool sizes for in-cluster service-to-service HTTP. Go's DefaultTransport
// keeps only 2 idle connections per host (DefaultMaxIdleConnsPerHost), so
// any client without its own Transport — http.DefaultClient, or
// &http.Client{Timeout: …} — RE-DIALS most requests once concurrency to an
// upstream exceeds 2. At load that was 12-18% of CPU in
// Transport.dialConnFor on the exchange and gateway (pprof 2026-10-15), and
// the same pattern sits on per-request paths elsewhere (publisher-adserver →
// SSP/adserver, SSP's http:// fallbacks).
const (
	MaxIdleConnsPerHost = 256
	MaxIdleConns        = 1024
	IdleConnTimeout     = 90 * time.Second
)

var tuneOnce sync.Once

// TuneDefaultTransport sizes http.DefaultTransport's idle pool for
// service-to-service traffic. Idempotent; call once at process start (done by
// config.Setup, which every service runs) BEFORE any client is used, so every
// DefaultClient / zero-Transport client in the process reuses connections.
func TuneDefaultTransport() {
	tuneOnce.Do(func() {
		if t, ok := http.DefaultTransport.(*http.Transport); ok {
			t.MaxIdleConns = MaxIdleConns
			t.MaxIdleConnsPerHost = MaxIdleConnsPerHost
			t.IdleConnTimeout = IdleConnTimeout
		}
	})
}
