//go:build e2e

package harness

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// scriptedRT returns a pre-scripted (resp, err) per call and records how many
// times it was invoked and the body it saw each time (to prove GetBody rewind).
type scriptedRT struct {
	steps     []step
	calls     int
	gotBodies []string
}

type step struct {
	status int  // 0 → return a transport error instead of a response
	err    bool // true → transport error
}

func (s *scriptedRT) RoundTrip(req *http.Request) (*http.Response, error) {
	i := s.calls
	s.calls++
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		s.gotBodies = append(s.gotBodies, string(b))
	} else {
		s.gotBodies = append(s.gotBodies, "")
	}
	st := s.steps[i]
	if st.err {
		return nil, io.ErrUnexpectedEOF
	}
	return &http.Response{StatusCode: st.status, Body: io.NopCloser(strings.NewReader(""))}, nil
}

func do(t *testing.T, steps []step, body string) (*scriptedRT, *http.Response, error) {
	t.Helper()
	base := &scriptedRT{steps: steps}
	rt := retryTransport{base: base}
	var req *http.Request
	if body != "" {
		req, _ = http.NewRequest(http.MethodPost, "http://x/y", strings.NewReader(body))
	} else {
		req, _ = http.NewRequest(http.MethodGet, "http://x/y", nil)
	}
	resp, err := rt.RoundTrip(req)
	return base, resp, err
}

func TestRetryTransport(t *testing.T) {
	cases := []struct {
		name      string
		steps     []step
		wantCalls int
		wantErr   bool
		wantCode  int
	}{
		{"transport error then success", []step{{err: true}, {status: 200}}, 2, false, 200},
		{"503 then 200", []step{{status: 503}, {status: 200}}, 2, false, 200},
		{"502 then 504 then 200", []step{{status: 502}, {status: 504}, {status: 200}}, 3, false, 200},
		{"500 is not retried", []step{{status: 500}, {status: 200}}, 1, false, 500},
		{"404 is not retried", []step{{status: 404}, {status: 200}}, 1, false, 404},
		{"200 first try", []step{{status: 200}}, 1, false, 200},
		{"exhaust on 503", []step{{status: 503}, {status: 503}, {status: 503}}, 3, false, 503},
		{"exhaust on transport error", []step{{err: true}, {err: true}, {err: true}}, 3, true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, resp, err := do(t, tc.steps, "")
			if base.calls != tc.wantCalls {
				t.Errorf("calls = %d, want %d", base.calls, tc.wantCalls)
			}
			if tc.wantErr {
				if err == nil {
					t.Errorf("want error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if resp.StatusCode != tc.wantCode {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantCode)
			}
		})
	}
}

// TestRetryTransportRewindsBody proves a POST body is replayed intact on retry
// (via GetBody), not truncated — otherwise the second attempt would send empty.
func TestRetryTransportRewindsBody(t *testing.T) {
	base, resp, err := do(t, []step{{status: 503}, {status: 200}}, "hello-body")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(base.gotBodies) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(base.gotBodies))
	}
	for i, b := range base.gotBodies {
		if b != "hello-body" {
			t.Errorf("attempt %d body = %q, want %q", i+1, b, "hello-body")
		}
	}
}
