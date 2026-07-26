//go:build e2e

package harness

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
)

// FireImpression hits the tracker's impression pixel endpoint as if a
// browser had loaded the served creative. The trace_id, campaign_id and
// clearing price come from the auction result so the NATS event chain
// matches what production would emit. Defaults bid_model to CPM —
// callers that need to exercise CPC/CPA/vCPM settle paths use
// FireImpressionWithModel instead.
func (h *Harness) FireImpression(t *testing.T, traceID, campaignID, creativeID, placementID, publisherID, advertiserID, currency string, price float64) {
	t.Helper()
	h.FireImpressionWithModel(t, traceID, campaignID, creativeID, placementID, publisherID, advertiserID, currency, price, "cpm")
}

// FireImpressionWithModel is FireImpression with an explicit bid_model
// query param. Production builds the impression URL via
// pkg/adserving.BuildImpressionURL which already stamps `bm`; tests
// bypass that path and must declare the model themselves so the billing
// engine routes reserve-vs-bill-immediately correctly.
func (h *Harness) FireImpressionWithModel(t *testing.T, traceID, campaignID, creativeID, placementID, publisherID, advertiserID, currency string, price float64, bidModel string) {
	t.Helper()
	url := fmt.Sprintf("%s/v1/t/imp?tid=%s&cid=%s&crid=%s&pid=%s&pubid=%s&advid=%s&price=%.4f&cur=%s&bm=%s",
		h.URLs.Tracker, traceID, campaignID, creativeID, placementID, publisherID, advertiserID, price, currency, bidModel)
	h.fireAndConsume(t, url, "impression")
}

// FireImpressionDeal is FireImpression with the winning deal's ID stamped
// as the `deal` query param — production stamps it via
// pkg/adserving.BuildImpressionURL when the serve context carries a deal.
// Reporting resolves the id to its deal TYPE (pg/pmp/preferred/open) so
// billing can apply the contract's deal_type_modifiers.
func (h *Harness) FireImpressionDeal(t *testing.T, traceID, campaignID, creativeID, placementID, publisherID, advertiserID, currency string, price float64, dealID string) {
	t.Helper()
	url := fmt.Sprintf("%s/v1/t/imp?tid=%s&cid=%s&crid=%s&pid=%s&pubid=%s&advid=%s&price=%.4f&cur=%s&bm=cpm&deal=%s",
		h.URLs.Tracker, traceID, campaignID, creativeID, placementID, publisherID, advertiserID, price, currency, dealID)
	h.fireAndConsume(t, url, "impression")
}

// FireClick hits the click redirect endpoint. Skips following the redirect
// since the e2e test only cares about the pixel firing and the NATS event,
// not the landing page.
func (h *Harness) FireClick(t *testing.T, traceID, campaignID, redir string) {
	t.Helper()
	url := fmt.Sprintf("%s/v1/t/click?tid=%s&cid=%s&redir=%s",
		h.URLs.Tracker, traceID, campaignID, redir)
	h.fireAndConsume(t, url, "click")
}

// FireView hits the viewability beacon with measurement params. The tracker
// computes IAB viewability server-side from dur/pct/areaPx; areaPx=0 uses
// the default 50% threshold. Tests assert the server's verdict via the
// returned X-IAB-Viewable header (or, for vCPM, by checking that the
// reservation did/didn't settle in the billing ledger).
func (h *Harness) FireView(t *testing.T, traceID, campaignID, placementID, publisherID string, durMs, pct int, areaPx int64) bool {
	t.Helper()
	url := fmt.Sprintf("%s/v1/t/view?tid=%s&cid=%s&pid=%s&pubid=%s&dur=%d&pct=%d&area=%d",
		h.URLs.Tracker, traceID, campaignID, placementID, publisherID, durMs, pct, areaPx)
	return h.fireAndConsumeReturningHeader(t, url, "view", "X-IAB-Viewable") == "1"
}

func (h *Harness) fireAndConsumeReturningHeader(t *testing.T, url, eventKind, header string) string {
	// Sign like production: tracker.signature_validation is enforced on the
	// local stack (2026-07-19 ratchet), so hand-built beacons must carry a
	// valid HMAC exactly as adserver-built ones do.
	url = adserving.SignURL(url, adserving.DefaultSigningKey)
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("fire %s: %v", eventKind, err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (e2e-harness)")
	req.Header.Set("Referer", "https://e2e.test/")
	resp, err := noRedirectClient.Do(req)
	if err != nil {
		t.Fatalf("fire %s call: %v", eventKind, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 500 {
		t.Fatalf("fire %s status %d", eventKind, resp.StatusCode)
	}
	return resp.Header.Get(header)
}

// FireConversion hits the conversion pixel endpoint for the given type and
// revenue. Used to test CPA settle flows.
func (h *Harness) FireConversion(t *testing.T, traceID, campaignID, convType, currency string, revenue float64) {
	t.Helper()
	url := fmt.Sprintf("%s/v1/t/conv?tid=%s&cid=%s&type=%s&rev=%.4f&cur=%s",
		h.URLs.Tracker, traceID, campaignID, convType, revenue, currency)
	h.fireAndConsume(t, url, "conversion")
}

// noRedirectClient is a request-scoped HTTP client that stops at the
// first 3xx so /v1/t/click (which 302s to the landing URL) doesn't end
// up dialling a fake domain like landing.test. Pre-existing h.HTTP
// follows redirects, which is the right default for ad-serve but wrong
// for the click tracker.
var noRedirectClient = &http.Client{
	Timeout: 5 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func (h *Harness) fireAndConsume(t *testing.T, url, eventKind string) {
	// Sign like production: tracker.signature_validation is enforced on the
	// local stack (2026-07-19 ratchet), so hand-built beacons must carry a
	// valid HMAC exactly as adserver-built ones do.
	url = adserving.SignURL(url, adserving.DefaultSigningKey)
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("fire %s: %v", eventKind, err)
	}
	// The tracker's real-time fraud check flags the default Go UA as a bot
	// (score 0.9, reason "bot_user_agent") and silently drops the event —
	// the pixel still returns 200 but no NATS publish happens, so billing
	// never accrues. Set a browser-shaped UA and a Referer so the harness
	// looks like a real impression. Without this, every tracker-driven
	// assertion silently passes by accident.
	req.Header.Set("User-Agent", "Mozilla/5.0 (e2e-harness)")
	req.Header.Set("Referer", "https://e2e.test/")
	resp, err := noRedirectClient.Do(req)
	if err != nil {
		t.Fatalf("fire %s call: %v", eventKind, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 500 {
		t.Fatalf("fire %s status %d", eventKind, resp.StatusCode)
	}
}
