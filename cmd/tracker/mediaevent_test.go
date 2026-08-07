package main

import (
	"net/url"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
)

// mediaMacroCtx is the full serve-time context the ad server / SSAI stitcher
// sign into a media beacon — the exact params the tracker must round-trip
// onto the typed event (zero slippage: stamped field in → same field out).
func mediaMacroCtx() adserving.MacroContext {
	return adserving.MacroContext{
		AuctionID:    "trace-media-1",
		CampaignID:   "li-9",
		CreativeID:   "cr-9",
		PlacementID:  "pl-9",
		PublisherID:  "pub-9",
		AdvertiserID: "acct-9",
		TrackerURL:   "http://tracker",
	}
}

// TestVideoEventFromQuery_StampsAttribution: the verified beacon params
// (cid/crid/pid/pubid/advid) land byte-for-byte on the VideoEvent — the same
// attribution treatment the impression pixel gets, so advertiser-tenant
// scoping on media_events works identically.
func TestVideoEventFromQuery_StampsAttribution(t *testing.T) {
	signed := adserving.BuildVideoEventURL(mediaMacroCtx(), "firstQuartile")
	u, err := url.Parse(signed)
	if err != nil {
		t.Fatalf("parse signed URL: %v", err)
	}
	q := u.Query()
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

	e := videoEventFromQuery(q, q.Get("tid"), q.Get("event"), now)
	if e.TraceID != "trace-media-1" || e.EventType != "firstQuartile" {
		t.Errorf("trace/event = %q/%q, want trace-media-1/firstQuartile", e.TraceID, e.EventType)
	}
	if e.CampaignID != "li-9" || e.CreativeID != "cr-9" || e.PlacementID != "pl-9" || e.PublisherID != "pub-9" {
		t.Errorf("attribution = %q/%q/%q/%q, want li-9/cr-9/pl-9/pub-9",
			e.CampaignID, e.CreativeID, e.PlacementID, e.PublisherID)
	}
	if e.AccountID != "acct-9" {
		t.Errorf("account_id = %q, want acct-9 (advid → AccountID, same as impressions)", e.AccountID)
	}
	if !e.Timestamp.Equal(now) {
		t.Errorf("timestamp = %v, want %v", e.Timestamp, now)
	}
}

// TestAudioEventFromQuery_StampsAttribution — audio twin of the video test.
func TestAudioEventFromQuery_StampsAttribution(t *testing.T) {
	signed := adserving.BuildAudioEventURL(mediaMacroCtx(), "complete")
	u, err := url.Parse(signed)
	if err != nil {
		t.Fatalf("parse signed URL: %v", err)
	}
	q := u.Query()

	e := audioEventFromQuery(q, q.Get("tid"), q.Get("event"), time.Now())
	if e.TraceID != "trace-media-1" || e.EventType != "complete" {
		t.Errorf("trace/event = %q/%q, want trace-media-1/complete", e.TraceID, e.EventType)
	}
	if e.CampaignID != "li-9" || e.CreativeID != "cr-9" || e.PlacementID != "pl-9" ||
		e.PublisherID != "pub-9" || e.AccountID != "acct-9" {
		t.Errorf("attribution = %q/%q/%q/%q acct=%q, want li-9/cr-9/pl-9/pub-9 acct-9",
			e.CampaignID, e.CreativeID, e.PlacementID, e.PublisherID, e.AccountID)
	}
}

// TestMediaEventFromQuery_LegacyBeaconEmptyAttribution: a pre-attribution
// beacon (no cid/.../advid params) still produces a valid event with empty
// attribution — additive rollout, old signed URLs keep working.
func TestMediaEventFromQuery_LegacyBeaconEmptyAttribution(t *testing.T) {
	q := url.Values{"tid": {"trace-legacy"}, "event": {"start"}}
	e := videoEventFromQuery(q, "trace-legacy", "start", time.Now())
	if e.TraceID != "trace-legacy" || e.EventType != "start" {
		t.Errorf("trace/event = %q/%q, want trace-legacy/start", e.TraceID, e.EventType)
	}
	if e.CampaignID != "" || e.AccountID != "" {
		t.Errorf("legacy beacon must yield empty attribution, got cid=%q acct=%q", e.CampaignID, e.AccountID)
	}
}
