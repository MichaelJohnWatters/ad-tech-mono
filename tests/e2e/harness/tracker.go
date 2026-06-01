//go:build e2e

package harness

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"
)

// FireImpression hits the tracker's impression pixel endpoint as if a
// browser had loaded the served creative. The trace_id, campaign_id and
// clearing price come from the auction result so the NATS event chain
// matches what production would emit.
func (h *Harness) FireImpression(t *testing.T, traceID, campaignID, creativeID, placementID, publisherID, advertiserID, currency string, price float64) {
	t.Helper()
	url := fmt.Sprintf("%s/v1/t/imp?tid=%s&cid=%s&crid=%s&pid=%s&pubid=%s&advid=%s&price=%.4f&cur=%s",
		h.URLs.Tracker, traceID, campaignID, creativeID, placementID, publisherID, advertiserID, price, currency)
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

// FireConversion hits the conversion pixel endpoint for the given type and
// revenue. Used to test CPA settle flows.
func (h *Harness) FireConversion(t *testing.T, traceID, campaignID, convType, currency string, revenue float64) {
	t.Helper()
	url := fmt.Sprintf("%s/v1/t/conv?tid=%s&cid=%s&type=%s&rev=%.4f&cur=%s",
		h.URLs.Tracker, traceID, campaignID, convType, revenue, currency)
	h.fireAndConsume(t, url, "conversion")
}

func (h *Harness) fireAndConsume(t *testing.T, url, eventKind string) {
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
	resp, err := h.HTTP.Do(req)
	if err != nil {
		t.Fatalf("fire %s call: %v", eventKind, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 500 {
		t.Fatalf("fire %s status %d", eventKind, resp.StatusCode)
	}
}
