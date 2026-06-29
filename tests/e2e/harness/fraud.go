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

// AddFraudBlocklist inserts a row into the global fraud_blocklists table
// (type ∈ ip|ua|domain|app_bundle). The tracker enforces these from a warm
// cache, so callers should RefreshAllCaches afterwards.
func (h *Harness) AddFraudBlocklist(t *testing.T, typ, value string) {
	t.Helper()
	if _, err := h.DB.Exec(
		`INSERT INTO fraud_blocklists (type, value, reason) VALUES ($1, $2, 'e2e')
		 ON CONFLICT DO NOTHING`, typ, value); err != nil {
		t.Fatalf("add fraud blocklist %s=%s: %v", typ, value, err)
	}
}

// ClearFraudBlocklist removes a blocklist row so it doesn't leak into other
// tests (the table is global).
func (h *Harness) ClearFraudBlocklist(t *testing.T, typ, value string) {
	t.Helper()
	if _, err := h.DB.Exec(`DELETE FROM fraud_blocklists WHERE type=$1 AND value=$2`, typ, value); err != nil {
		t.Fatalf("clear fraud blocklist %s=%s: %v", typ, value, err)
	}
}

// FireImpressionUA fires an impression pixel with a custom User-Agent and
// reports whether the tracker's fraud check blocked it (via the
// X-Dev-Fraud-Blocked debug header). Used to exercise UA blocklist rules.
func (h *Harness) FireImpressionUA(t *testing.T, traceID, campaignID, userAgent string) bool {
	t.Helper()
	url := fmt.Sprintf("%s/v1/t/imp?tid=%s&cid=%s&price=1.0000&cur=USD",
		h.URLs.Tracker, traceID, campaignID)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("fire imp UA: %v", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", "https://e2e.test/")
	resp, err := noRedirectClient.Do(req)
	if err != nil {
		t.Fatalf("fire imp UA call: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.Header.Get("X-Dev-Fraud-Blocked") == "1"
}
