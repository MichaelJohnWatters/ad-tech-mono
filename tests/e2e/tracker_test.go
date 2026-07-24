//go:build e2e

// Tracker pixel paths — every endpoint should respond cleanly and (for
// state-changing pixels) propagate via NATS to the reporting service.
package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func itoa(i int) string { return strconv.Itoa(i) }

// TestTrackerClickRedirects — the click endpoint must 302 to the redir URL,
// with the trace id appended (?adtech_tid=) so the landing page can attribute
// the click (cmd/tracker appendTraceQuery). We disable follow-redirect on the
// client so we can inspect the 302 itself.
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
	if loc == nil {
		t.Fatal("no Location header on click 302")
	}
	// Base URL preserved; trace id appended for attribution.
	if base := loc.Scheme + "://" + loc.Host + loc.Path; base != landing {
		t.Errorf("Location base = %q, want %q", base, landing)
	}
	if loc.Query().Get("adtech_tid") == "" {
		t.Errorf("Location missing adtech_tid attribution param: %s", loc)
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

// TestTrackerViewabilityBeacon — viewability endpoint returns 204 with no
// body and stamps X-IAB-Viewable when the inputs cross the IAB threshold
// (>=50% pixels for >=1s on standard ads). Server-side computation is the
// authority; client's bool is not consulted.
func TestTrackerViewabilityBeacon(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "tracker-view")

	cases := []struct {
		name         string
		dur          int
		pct          int
		wantViewable bool
	}{
		{"clearly viewable", 2000, 75, true},
		{"below time threshold", 500, 100, false},
		{"below pct threshold", 2000, 40, false},
		{"right at threshold", 1000, 50, true},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Spaces in c.name would land unescaped in the URL and trip
			// Go's http.NewRequest before it reaches the server.
			tid := "trk-view-" + strings.ReplaceAll(c.name, " ", "-")
			url := h.URLs.Tracker + "/v1/t/view?tid=" + tid + "&cid=" + w.Campaign.ID +
				"&dur=" + itoa(c.dur) + "&pct=" + itoa(c.pct)
			resp := get(t, h, url)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				t.Fatalf("viewability status = %d, want 204", resp.StatusCode)
			}
			got := resp.Header.Get("X-IAB-Viewable") == "1"
			if got != c.wantViewable {
				t.Errorf("case %d %s: X-IAB-Viewable=%v, want %v", i, c.name, got, c.wantViewable)
			}
		})
	}
}

// TestTrackerHMACStrictMode — flip tracker.signature_validation to true,
// hit /v1/t/imp without a sig param and assert 403. Then flip back and
// confirm the same request succeeds. Proves the live-config knob gates
// the HMAC enforcement path (warn-only by default in dev, hard-reject
// when ops ratchet to strict).
func TestTrackerHMACStrictMode(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "trk-hmac")

	const pod = "tracker-0"
	const key = "tracker.signature_validation"

	// Force baseline to "false" so the next assertion is unambiguous —
	// don't rely on whatever a prior test happened to leave behind.
	// Cleanup also resets to "false" (the schema default), not to whatever
	// the row held before — leftover "true" from a previous failed run
	// would otherwise poison every later test that hits a tracker
	// endpoint without a sig param.
	t.Cleanup(func() {
		h.SetConfigForPod(t, key, "false", pod)
	})
	h.SetConfigForPod(t, key, "false", pod)

	// Both flips propagate to each tracker replica via NATS invalidate with
	// the config manager's 30s poll as the fallback — so each direction is
	// asserted through a retry window that covers a full poll cycle (a
	// leftover strict=true from a prior failed run makes the baseline race
	// real, not theoretical). Fresh trace ids per attempt keep dedup out of
	// the picture.
	harness.WaitFor(t, 35*time.Second, "tracker accepts unsigned (strict=false baseline)", func() bool {
		url := h.URLs.Tracker + fmt.Sprintf("/v1/t/imp?tid=trk-hmac-base-%d&cid=%s", time.Now().UnixNano(), w.Campaign.ID)
		resp := get(t, h, url)
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})

	h.SetConfigForPod(t, key, "true", pod)

	harness.WaitFor(t, 35*time.Second, "tracker picks up signature_validation=true", func() bool {
		url := h.URLs.Tracker + fmt.Sprintf("/v1/t/imp?tid=trk-hmac-strict-%d&cid=%s", time.Now().UnixNano(), w.Campaign.ID)
		resp := get(t, h, url)
		resp.Body.Close()
		return resp.StatusCode == http.StatusForbidden
	})

	// Accept-under-strict: a correctly HMAC-signed pixel must be accepted even
	// with validation on — proving strict rejects forgeries, not all traffic.
	// (The reject above is unsigned → 403; this is the same endpoint, signed.)
	signed := adserving.SignURL(
		h.URLs.Tracker+fmt.Sprintf("/v1/t/imp?tid=trk-hmac-ok-%d&cid=%s", time.Now().UnixNano(), w.Campaign.ID),
		adserving.DefaultSigningKey)
	resp := get(t, h, signed)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("strict HMAC: a validly-signed pixel was rejected (status %d, want 200)", resp.StatusCode)
	}
}

func get(t *testing.T, h *harness.Harness, url string) *http.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	// The tracker's fraud checker treats the default Go UA as a bot —
	// it would silently 204 the request without firing the view event
	// path. Set a browser-shaped UA + Referer so the harness exercises
	// the real flow. Same pattern as harness.fireAndConsume.
	req.Header.Set("User-Agent", "Mozilla/5.0 (e2e-harness)")
	req.Header.Set("Referer", "https://e2e.test/")
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
