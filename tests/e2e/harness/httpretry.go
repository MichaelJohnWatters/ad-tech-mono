//go:build e2e

package harness

import (
	"net/http"
	"time"
)

// retryTransport retries transport-level failures — connection refused / EOF /
// reset when the port-forward tunnel to a service flaps because a pod is
// briefly CPU-starved under full-suite load. A single-shot HTTP call catching
// one of these used to fast-fail a test in SETUP (well before its real
// WaitFor), and the failure landed on a different arbitrary test each run.
//
// It retries ONLY when RoundTrip returns an error — i.e. no response was
// received, so the server never processed the request and re-sending is safe
// for any method. A real non-2xx response is passed straight through untouched,
// so genuine failures stay loud. Request bodies are rewound via GetBody, which
// http.NewRequest sets automatically for the bytes/strings readers the harness
// and tests use; a non-replayable body aborts the retry rather than truncating.
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
					return resp, err // can't safely replay; surface the last error
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
		if err == nil {
			return resp, nil
		}
	}
	return resp, err
}

// newHTTPClient builds an *http.Client with the shared flap-retrying transport.
func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: retryTransport{}}
}
