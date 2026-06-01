//go:build e2e

// Tracker pixel paths — every endpoint should respond cleanly and (for
// state-changing pixels) propagate via NATS to the reporting service.
package e2e

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// TestTrackerClickRedirects — the click endpoint must 302 to the redir URL.
// We disable follow-redirect on the client so we can inspect the 302 itself.
func TestTrackerClickRedirects(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "tracker-click")

	landing := "https://example.test/landing"
	url := h.URLs.Tracker + "/v1/t/click?tid=trk-click-1&cid=" + w.Campaign.ID + "&redir=" + landing
	resp := getNoFollow(t, url)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("click status = %d, want 302", resp.StatusCode)
	}
	loc, _ := resp.Location()
	if loc == nil || loc.String() != landing {
		t.Errorf("Location header = %v, want %q", loc, landing)
	}
}

// TestTrackerConversionPixel — POST tracker conversion, expect 200 + GIF.
// Doesn't assert NATS arrival here (covered loosely by the billing test
// in step 08), just that the endpoint serves the pixel.
func TestTrackerConversionPixel(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "tracker-conv")

	url := h.URLs.Tracker + "/v1/t/conv?tid=trk-conv-1&cid=" + w.Campaign.ID + "&type=purchase&rev=29.99&cur=USD"
	resp := get(t, h, url)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("conversion status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) == 0 {
		t.Error("expected 1x1 GIF body, got empty response")
	}
}

// TestTrackerViewabilityBeacon — viewability endpoint returns 204 with no body.
func TestTrackerViewabilityBeacon(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "tracker-view")

	url := h.URLs.Tracker + "/v1/t/view?tid=trk-view-1&cid=" + w.Campaign.ID + "&dur=2000&pct=75"
	resp := get(t, h, url)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("viewability status = %d, want 204", resp.StatusCode)
	}
}

// TestTrackerHMACStrictMode — when tracker.signature_validation is true,
// requests without a valid sig must be rejected. Currently the tracker
// runs in dev mode (signature_validation=false), so this is skipped until
// we either flip via config-manager mid-test or add a "strict" flag to the
// test harness. Sketches the assertion shape.
func TestTrackerHMACStrictMode(t *testing.T) {
	t.Skip("HMAC strict mode requires flipping tracker.signature_validation via config-manager mid-test; build a config-write helper before enabling")
}

func get(t *testing.T, h *harness.Harness, url string) *http.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := h.HTTP.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

// getNoFollow is a one-off no-redirect HTTP client used by the click test
// so we can observe the 302 directly rather than following to a landing
// page that may 404 in the test environment.
func getNoFollow(t *testing.T, url string) *http.Response {
	t.Helper()
	client := &http.Client{
		Timeout: 3 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}
