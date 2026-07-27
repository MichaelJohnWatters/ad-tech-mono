//go:build e2e

package harness

import (
	"io"
	"net/http"
	"time"
)

// retryTransport retries transport-level failures — connection refused / EOF /
// reset when the port-forward tunnel to a service flaps because a pod is
// briefly CPU-starved under full-suite load. A single-shot HTTP call catching
// one of these used to fast-fail a test in SETUP (well before its real
// WaitFor), and the failure landed on a different arbitrary test each run.
//
// It retries on two transient conditions:
//   - a RoundTrip error — no response received, so the server never processed
//     the request and re-sending is safe for any method; and
//   - a 502/503/504 response — pure-infrastructure transients (bad gateway /
//     unavailable / gateway timeout) from the tunnel or a briefly-busy pod
//     under load, which NO test legitimately asserts as a success. A 500 or
//     any 4xx is passed straight through untouched, so genuine application
//     failures (and expected 400/401/404/409/429) stay loud and immediate.
//
// Request bodies are rewound via GetBody, which http.NewRequest sets
// automatically for the bytes/strings readers the harness and tests use; a
// non-replayable body aborts the retry rather than truncating.
//
// This is the shared, systemic version of the inline retry that refreshOne /
// putConfig / DeleteConfig / reseed each grew independently — wiring it onto
// every harness http.Client covers the logged-in cookie clients too (which
// per-helper retries never could).
type retryTransport struct{ base http.RoundTripper }

func (rt retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := rt.base
	if base == nil {
		base = http.DefaultTransport
	}
	const attempts = 3
	var resp *http.Response
	var err error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			if req.Body != nil {
				if req.GetBody == nil {
					return resp, err // can't safely replay; surface the last result
				}
				b, gerr := req.GetBody()
				if gerr != nil {
					return resp, err
				}
				req.Body = b
			}
			time.Sleep(time.Second)
		}
		resp, err = base.RoundTrip(req)
		if err != nil {
			continue // transport error → retry
		}
		// Retry pure-infra transient statuses, but never on the final attempt
		// (return whatever we have so the caller sees the real status).
		if i < attempts-1 && isTransientStatus(resp.StatusCode) {
			io.Copy(io.Discard, resp.Body) //nolint:errcheck // draining for conn reuse
			resp.Body.Close()
			continue
		}
		return resp, nil
	}
	return resp, err
}

// isTransientStatus reports whether a status is a pure-infrastructure transient
// safe to retry: bad gateway, service unavailable, gateway timeout. Explicitly
// NOT 500 (could be a real bug) or any 4xx (many are expected assertions).
func isTransientStatus(code int) bool {
	return code == http.StatusBadGateway ||
		code == http.StatusServiceUnavailable ||
		code == http.StatusGatewayTimeout
}

// newHTTPClient builds an *http.Client with the shared flap-retrying transport.
func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: retryTransport{}}
}

// NewHTTPClient is the exported form of newHTTPClient, for tests (package e2e)
// that build their own client instead of reusing h.HTTP but still want the
// flap-retrying transport.
func NewHTTPClient(timeout time.Duration) *http.Client { return newHTTPClient(timeout) }

// RetryTransport returns the shared flap-retrying RoundTripper, for tests that
// need a custom *http.Client (e.g. a no-follow CheckRedirect) yet still want
// transport-level retry. Set it as the client's Transport.
func RetryTransport() http.RoundTripper { return retryTransport{} }
