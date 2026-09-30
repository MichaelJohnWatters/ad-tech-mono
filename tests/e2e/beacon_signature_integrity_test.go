//go:build e2e

// Beacon signature INTEGRITY: prove the tracker's HMAC actually BINDS the signed
// fields, not merely that an unsigned beacon is rejected (that gap is covered by
// TestTrackerHMACStrictMode). The money-critical claim under audit is that a
// beacon whose billing-relevant params (price, pubid) have been tampered — while
// keeping the ORIGINAL server signature — is rejected (403) and never recorded,
// so an attacker can neither inflate spend nor redirect a publisher's payout.
//
// The signed URL here is genuinely SERVER-minted: we serve a real ad and take
// the ad server's own ImpressionURL (adserving.BuildImpressionURL signs every
// query param, price + pubid included). We then string-mutate ONE signed param,
// keep the original sig, and fire it with a browser-shaped UA + Referer (so the
// tracker's real-time fraud check doesn't bot-drop the fire and mask the
// signature verdict). A positive control fires the UNTAMPERED URL and asserts it
// records exactly one impression (and a replay stays at one), proving the gate
// rejects TAMPERING specifically, not all traffic.
package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// serveSignedImpressionURL builds a basic world, serves one real ad, and returns
// the ad server's own signed impression URL RE-TARGETED at the reachable tracker
// host. The HMAC covers path+params (not host), so re-hosting keeps it valid —
// same technique as TestARAAdServerBakesSourceBeacon. A fresh trace id is baked
// per call so dedup from prior runs can't interfere.
func serveSignedImpressionURL(t *testing.T, h *harness.Harness, w harness.World, traceID string) string {
	t.Helper()
	resp := h.ServeAdResponse(t, models.ServeRequest{
		TraceID:       traceID,
		CampaignID:    w.Campaign.ID,
		CreativeID:    w.Campaign.CreativeID,
		PlacementID:   w.Placement.ID,
		PublisherID:   w.Publisher.ID,
		AdvertiserID:  w.AdvAcc.ID,
		ClearingPrice: 2.50,
		Currency:      "USD",
		SiteDomain:    w.Publisher.Domain,
		Width:         300,
		Height:        250,
		BidModel:      "cpm",
	})
	if resp.ImpressionURL == "" {
		t.Fatal("serve returned no impression_url — cannot obtain a server-signed beacon")
	}
	u, err := url.Parse(resp.ImpressionURL)
	if err != nil {
		t.Fatalf("parse served impression_url: %v", err)
	}
	// Re-target the reachable tracker host; sig is over path+params, so it holds.
	return h.URLs.Tracker + u.Path + "?" + u.RawQuery
}

// mutateSignedParam returns beaconURL with a single query param overwritten while
// KEEPING the original sig — the forgery shape. It does NOT re-sign.
func mutateSignedParam(t *testing.T, beaconURL, param, newValue string) string {
	t.Helper()
	u, err := url.Parse(beaconURL)
	if err != nil {
		t.Fatalf("parse beacon url: %v", err)
	}
	q := u.Query()
	if q.Get("sig") == "" {
		t.Fatalf("beacon url carries no sig to defeat: %s", beaconURL)
	}
	if q.Get(param) == "" {
		t.Fatalf("beacon url carries no %s param to tamper: %s", param, beaconURL)
	}
	q.Set(param, newValue) // sig left untouched → HMAC no longer matches the body
	u.RawQuery = q.Encode()
	return u.String()
}

// fireBeaconStatus fires a beacon URL AS-IS (no signing) with a browser-shaped
// UA + Referer and returns the HTTP status. The browser shape matters: the
// default Go UA trips the tracker's bot filter, which returns 200 without
// recording — that would mask the signature verdict entirely.
func fireBeaconStatus(t *testing.T, url string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build beacon request: %v", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (e2e-beacon-integrity)")
	req.Header.Set("Referer", "https://e2e.test/")
	resp, err := harness.NewHTTPClient(5 * time.Second).Do(req)
	if err != nil {
		t.Fatalf("fire beacon: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// enforceStrictSignatures turns tracker.signature_validation on for the run and
// deletes the override on the way out (reverting to the DEPLOYED baseline, which
// helm sets true — same discipline as TestTrackerHMACStrictMode). It waits until
// strict has actually propagated by asserting an UNSIGNED beacon 403s.
func enforceStrictSignatures(t *testing.T, h *harness.Harness) {
	t.Helper()
	const pod = "tracker-0"
	const key = "tracker.signature_validation"
	t.Cleanup(func() { h.DeleteConfig(t, key) })
	h.SetConfigForPod(t, key, "true", pod)
	harness.WaitFor(t, 40*time.Second, "tracker enforcing signature_validation=true", func() bool {
		u := h.URLs.Tracker + fmt.Sprintf("/v1/t/imp?tid=beacon-sig-probe-%d", time.Now().UnixNano())
		return fireBeaconStatus(t, u) == http.StatusForbidden // unsigned → 403 once strict is live
	})
}

// TestBeaconRejectsTamperedPrice: an attacker inflates the billing price on a
// genuinely server-signed impression beacon (×10) while keeping the original
// sig. The tracker must 403 and record NO impression for that trace — a tampered
// price can drive no spend.
func TestBeaconRejectsTamperedPrice(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "beacon-price")
	enforceStrictSignatures(t, h)

	trace := fmt.Sprintf("beacon-tamper-price-%d", time.Now().UnixNano())
	signed := serveSignedImpressionURL(t, h, w, trace)

	// price=2.5000 → 25.0000 (×10), sig unchanged.
	tampered := mutateSignedParam(t, signed, "price", "25.0000")
	if st := fireBeaconStatus(t, tampered); st != http.StatusForbidden {
		t.Fatalf("price-tampered beacon got status %d, want 403 (HMAC must bind price)", st)
	}

	// Give any (erroneous) async publish a chance to land, then assert nothing did.
	time.Sleep(2 * time.Second)
	if got := h.ClickHouseScalar(t,
		fmt.Sprintf("SELECT count() FROM adtech.impressions WHERE trace_id='%s'", trace)); got != 0 {
		t.Errorf("price-tampered beacon produced %d impression rows for trace %s; want 0 (must never bill)", got, trace)
	}
}

// TestBeaconRejectsSpoofedPublisher: an attacker swaps pubid to a DIFFERENT
// publisher's UUID on a server-signed beacon, keeping the original sig, to
// redirect the impression (and its revenue) to another publisher. The tracker
// must 403 and record NO impression for that trace.
func TestBeaconRejectsSpoofedPublisher(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "beacon-pub")
	enforceStrictSignatures(t, h)

	// A second, real publisher to spoof the impression onto — the money-redirect
	// target. Its UUID stands in for "some other publisher's id".
	victimPubAcc := h.CreatePublisher(t, "e2e-beacon-victimpub")
	victimPub := h.AddPublisher(t, victimPubAcc, "e2e-beacon-victimpub", "victim-beacon.test")
	if victimPub.ID == w.Publisher.ID {
		t.Fatal("victim publisher id collided with the served publisher; test would be a no-op")
	}

	trace := fmt.Sprintf("beacon-spoof-pub-%d", time.Now().UnixNano())
	signed := serveSignedImpressionURL(t, h, w, trace)

	tampered := mutateSignedParam(t, signed, "pubid", victimPub.ID)
	if st := fireBeaconStatus(t, tampered); st != http.StatusForbidden {
		t.Fatalf("publisher-spoofed beacon got status %d, want 403 (HMAC must bind pubid)", st)
	}

	time.Sleep(2 * time.Second)
	if got := h.ClickHouseScalar(t,
		fmt.Sprintf("SELECT count() FROM adtech.impressions WHERE trace_id='%s'", trace)); got != 0 {
		t.Errorf("publisher-spoofed beacon produced %d impression rows for trace %s; want 0 (revenue must not be redirectable)", got, trace)
	}
}

// TestBeaconSignedImpressionRecordsOnce is the POSITIVE CONTROL for the two
// tamper tests above: the UNTAMPERED server-signed beacon is accepted (200) and
// records EXACTLY ONE impression, and a replay of it STAYS at one (dedup). This
// proves the gate rejects tampering specifically — not all traffic — and that
// the tamper tests' "0 rows" is a real rejection, not a broken serve path.
func TestBeaconSignedImpressionRecordsOnce(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "beacon-ok")
	enforceStrictSignatures(t, h)

	trace := fmt.Sprintf("beacon-ok-%d", time.Now().UnixNano())
	signed := serveSignedImpressionURL(t, h, w, trace)

	if st := fireBeaconStatus(t, signed); st != http.StatusOK {
		t.Fatalf("untampered server-signed beacon got status %d, want 200", st)
	}
	waitCH(t, h,
		fmt.Sprintf("SELECT count() FROM adtech.impressions WHERE trace_id='%s'", trace),
		"untampered signed impression recorded")

	// Replay the identical signed URL: dedup must keep it at exactly one row.
	if st := fireBeaconStatus(t, signed); st != http.StatusOK {
		t.Fatalf("replay of signed beacon got status %d, want 200", st)
	}
	time.Sleep(2 * time.Second)
	if got := h.ClickHouseScalar(t,
		fmt.Sprintf("SELECT count() FROM adtech.impressions WHERE trace_id='%s'", trace)); got != 1 {
		t.Errorf("signed impression trace %s has %d rows after a replay; want exactly 1 (accept once, dedup the rest)", trace, got)
	}
}
