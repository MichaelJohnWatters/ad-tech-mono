package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/fraud"
)

// newTestGate builds a mediaEventGate wired to in-memory deps (memory L2 for
// dedup, a memory bus so the fire-and-forget publishRejected never panics) with
// the real HMAC key and fraud checker, so the tests exercise the genuine gate.
func newTestGate(sigValidation bool) mediaEventGate {
	log := slog.New(slog.NewTextHandler(nopWriter{}, nil))
	bus := events.NewMemoryBus()
	pub := &eventPublisher{bus: bus, typed: events.NewPublisher(bus, log), log: log}
	return mediaEventGate{
		sigKeys:       func() []string { return []string{adserving.DefaultSigningKey} },
		sigValidation: func() bool { return sigValidation },
		expValidation: func() bool { return true },
		fraud:         fraud.NewRealTimeChecker(fraud.DefaultConfig()),
		dedup:         NewDedup(cache.NewMemoryL2(), dedupTTL, dedupOn, log),
		publisher:     pub,
	}
}

// signedVideoBeacon returns the real HMAC-signed /v1/t/video URL the SSAI
// stitcher / publisher-adserver emit for a quartile — the exact contract the
// gate must accept.
func signedVideoBeacon(traceID, event string) string {
	return adserving.BuildVideoEventURL(adserving.MacroContext{
		AuctionID: traceID, CampaignID: "li", CreativeID: "cr",
		PlacementID: "pl", PublisherID: "pub", TrackerURL: "http://tracker",
	}, event)
}

// playerRequest shapes a beacon request like a real player/server-side beacon:
// a browser UA + Referer so the fraud check treats it as legitimate (matches
// SSAI's fireBeacon and the simulator).
func playerRequest(url string) *http.Request {
	r := httptest.NewRequest("GET", url, nil)
	r.Header.Set("User-Agent", "Mozilla/5.0 (adtech-test)")
	r.Header.Set("Referer", "https://player.example/")
	return r
}

func nopLog() *slog.Logger { return slog.New(slog.NewTextHandler(nopWriter{}, nil)) }

// TestMediaGate_UnsignedRejectedInStrictMode: with tracker.signature_validation
// on, an unsigned /v1/t/video beacon (the spoof: GET /v1/t/video?event=complete)
// is 403'd and not recorded.
func TestMediaGate_UnsignedRejectedInStrictMode(t *testing.T) {
	g := newTestGate(true)
	rec := httptest.NewRecorder()
	r := playerRequest("http://tracker/v1/t/video?tid=trace-spoof&event=complete")
	if ok, _ := g.allow(rec, r, "video", "complete", "trace-spoof", nopLog()); ok {
		t.Fatal("unsigned beacon allowed in strict mode")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

// TestMediaGate_SignedAllowedThenDeduped: a correctly-signed beacon passes, and
// the SAME quartile fired again on the same trace (hls.js/dash.js prefetch or
// seek-back) is dropped — the over-count fix.
func TestMediaGate_SignedAllowedThenDeduped(t *testing.T) {
	g := newTestGate(true)
	url := signedVideoBeacon("trace-a", "start")

	rec1 := httptest.NewRecorder()
	if ok, _ := g.allow(rec1, playerRequest(url), "video", "start", "trace-a", nopLog()); !ok {
		t.Fatalf("signed beacon rejected: status=%d", rec1.Code)
	}

	rec2 := httptest.NewRecorder()
	if ok, _ := g.allow(rec2, playerRequest(url), "video", "start", "trace-a", nopLog()); ok {
		t.Fatal("re-fired quartile not deduped (over-count)")
	}
	if rec2.Code != http.StatusNoContent {
		t.Errorf("dedup drop status = %d, want 204", rec2.Code)
	}
}

// TestMediaGate_DistinctQuartilesEachRecordOnce: distinct quartiles on one trace
// each record once (per-quartile-per-trace dedup key), so a legit ad's full
// quartile set is never collapsed to a single event.
func TestMediaGate_DistinctQuartilesEachRecordOnce(t *testing.T) {
	g := newTestGate(true)
	for _, ev := range []string{"start", "firstQuartile", "midpoint", "thirdQuartile", "complete"} {
		rec := httptest.NewRecorder()
		if ok, _ := g.allow(rec, playerRequest(signedVideoBeacon("trace-b", ev)), "video", ev, "trace-b", nopLog()); !ok {
			t.Errorf("quartile %q rejected on first fire: status=%d", ev, rec.Code)
		}
	}
	// A re-fire of any one quartile is still dropped.
	rec := httptest.NewRecorder()
	if ok, _ := g.allow(rec, playerRequest(signedVideoBeacon("trace-b", "midpoint")), "video", "midpoint", "trace-b", nopLog()); ok {
		t.Error("re-fired midpoint not deduped")
	}
}

// TestMediaGate_FraudBlockedSilent204: a bot-UA beacon is blocked by the fraud
// check and answered with a silent 204 (don't reveal detection), not recorded.
func TestMediaGate_FraudBlockedSilent204(t *testing.T) {
	g := newTestGate(false) // sig off so we isolate the fraud path
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", signedVideoBeacon("trace-bot", "start"), nil)
	r.Header.Set("User-Agent", "Googlebot/2.1 (+http://www.google.com/bot.html)")
	if ok, _ := g.allow(rec, r, "video", "start", "trace-bot", nopLog()); ok {
		t.Fatal("bot beacon allowed")
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("fraud drop status = %d, want silent 204", rec.Code)
	}
}

// TestMediaGate_UnsignedAllowedInDevMode: with signature_validation off (dev
// default), an unsigned beacon is warned-but-allowed — mirrors the impression
// pixel so dev pipelines that don't yet sign keep flowing.
func TestMediaGate_UnsignedAllowedInDevMode(t *testing.T) {
	g := newTestGate(false)
	rec := httptest.NewRecorder()
	r := playerRequest("http://tracker/v1/t/audio?tid=trace-dev&event=start")
	if ok, _ := g.allow(rec, r, "audio", "start", "trace-dev", nopLog()); !ok {
		t.Fatalf("unsigned beacon rejected in dev mode: status=%d", rec.Code)
	}
}
