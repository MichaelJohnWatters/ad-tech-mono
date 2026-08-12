//go:build e2e

package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// ServeAd POSTs a ServeRequest to the ad server. Used by the freq cap test
// — calling the ad server directly with a stable user_id + campaign_id N
// times verifies the Redis counter blocks the Nth call regardless of which
// auction path produced it.
//
// Returns the ad server's HTTP status so callers can distinguish 200 (served)
// from 429 (freq cap exceeded).
func (h *Harness) ServeAd(t *testing.T, req models.ServeRequest) int {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal serve request: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URLs.AdServer+routes.AdServe, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build serve request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := h.HTTP.Do(httpReq)
	if err != nil {
		t.Fatalf("serve call: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// ServeAdHTML POSTs a ServeRequest and returns the rendered HTML from the
// ServeResponse (empty on non-200). Used to assert dynamic-creative assembly.
func (h *Harness) ServeAdHTML(t *testing.T, req models.ServeRequest) string {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal serve request: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URLs.AdServer+routes.AdServe, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build serve request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := h.HTTP.Do(httpReq)
	if err != nil {
		t.Fatalf("serve call: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var out models.ServeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode serve response: %v", err)
	}
	return out.HTML
}

// ServeAdResponse POSTs a ServeRequest and returns the full decoded
// ServeResponse (for assertions on fields beyond HTML, e.g. ara_source_url).
// Fails the test on a non-200.
func (h *Harness) ServeAdResponse(t *testing.T, req models.ServeRequest) models.ServeResponse {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal serve request: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URLs.AdServer+routes.AdServe, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build serve request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := h.HTTP.Do(httpReq)
	if err != nil {
		t.Fatalf("serve call: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("serve = %d, want 200", resp.StatusCode)
	}
	var out models.ServeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode serve response: %v", err)
	}
	return out
}
