//go:build e2e

package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/fraud"
)

// SetAdsTxt upserts an ads_txt_cache row for a publisher domain. entries is
// marshalled with the same struct the exchange loader decodes, so it
// round-trips. The exchange warm-caches this table; callers should
// RefreshAllCaches afterwards.
func (h *Harness) SetAdsTxt(t *testing.T, domain string, entries []fraud.AdsTxtEntry, status string) {
	t.Helper()
	b, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("marshal ads.txt entries: %v", err)
	}
	const q = `
INSERT INTO ads_txt_cache (domain, entries, last_fetched, status)
VALUES ($1, $2, now(), $3)
ON CONFLICT (domain) DO UPDATE SET entries=EXCLUDED.entries, last_fetched=now(), status=EXCLUDED.status`
	if _, err := h.DB.Exec(q, domain, b, status); err != nil {
		t.Fatalf("set ads.txt for %s: %v", domain, err)
	}
}

// ClearAdsTxt removes an ads_txt_cache row (the table is global).
func (h *Harness) ClearAdsTxt(t *testing.T, domain string) {
	t.Helper()
	if _, err := h.DB.Exec(`DELETE FROM ads_txt_cache WHERE domain=$1`, domain); err != nil {
		t.Fatalf("clear ads.txt for %s: %v", domain, err)
	}
}

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

// FireImpressionFromIP fires an impression pixel spoofing the client IP via
// X-Forwarded-For (the tracker derives the fraud-check IP from XFF behind the
// proxy). Uses a normal browser UA so only the IP rule is under test. Reports
// whether the tracker's fraud check blocked it.
func (h *Harness) FireImpressionFromIP(t *testing.T, traceID, campaignID, ip string) bool {
	t.Helper()
	url := fmt.Sprintf("%s/v1/t/imp?tid=%s&cid=%s&price=1.0000&cur=USD",
		h.URLs.Tracker, traceID, campaignID)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("fire imp IP: %v", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (e2e)")
	req.Header.Set("Referer", "https://e2e.test/")
	req.Header.Set("X-Forwarded-For", ip)
	resp, err := noRedirectClient.Do(req)
	if err != nil {
		t.Fatalf("fire imp IP call: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.Header.Get("X-Dev-Fraud-Blocked") == "1"
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
