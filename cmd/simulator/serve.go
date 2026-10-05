package main

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/simulator/request"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/vast"
)

// This file makes the CLI mirror the web simulator: each channel goes through
// the SAME first-party endpoint the corresponding web-UI tab uses (SSP serve
// for display, publisher-adserver for video/native/audio), and the CLI fires
// the beacon URLs the SERVER returned — exactly what a browser/player does.
// The CLI never builds its own beacons on this path, so what it exercises is
// byte-for-byte the production flow the web UI shows.
//
// Request params come from persona.QueryParams — the same shared source of
// truth the web UI's realismParams() mirrors — so consent/identity/geo/device
// signals are identical across CLI and web.

// endpoints holds the service URLs the mirror path dials.
type endpoints struct {
	SSP   string
	PubAd string
}

// placementKeyFor maps a channel to the simulator publisher's placement for
// that format (seeded in profiles/publishers/standard.yaml). Matches the
// per-channel placement lists the web UI uses.
func placementKeyFor(ch request.Channel) string {
	switch ch {
	case request.Native:
		return "pl-sim-native"
	case request.Video:
		return "pl-sim-video"
	case request.Audio:
		return "pl-sim-audio"
	default:
		return "pl-sim-mpu"
	}
}

// serveMirror runs one request through the first-party flow for its channel and
// fires the server-returned beacons. Returns whether an ad was served. The
// placementKey selects which seeded placement (and thus publisher) the request
// targets — the caller picks it so traffic spreads across all publishers.
func serveMirror(client *http.Client, u endpoints, persona request.Persona, ch request.Channel, pod int, p profile, rng *rand.Rand, traceparent, placementKey string) (bool, error) {
	params := persona.QueryParams(placementKey, ch)
	// QueryParams derives a deterministic per-persona user_id (stable identity
	// for the trace explorer / web UI). For traffic generation that collapses
	// all volume onto the handful of personas and trips the ad server's
	// per-user-per-campaign frequency cap almost immediately, starving fill.
	// Give each served impression a fresh high-cardinality user so fill reflects
	// a realistic audience spread. Anonymous personas (no user_id) stay
	// anonymous — they carry no identifier and bypass the cap by design.
	//
	// Themed personas (UserPool > 0) instead draw from a small STABLE pool:
	// the same users recur, so repeat visits accumulate the behaviour signals
	// that earn segment membership. Random ids would make min_count>1 rules
	// unreachable — every visit would look like a brand-new user.
	//
	// --user-pool N (density load runs) overrides both: user ids come from
	// the SAME "synth-user-%06d" universe cmd/seed --synthetic-users seeds
	// memberships for, so bid-time audience lookups get realistic HITS
	// (SMEMBERS with members + include-segment matches) instead of universal
	// misses against a random 4-billion-id space.
	if params.Get(request.ParamUserID) != "" {
		switch {
		case loadUserPool > 0:
			params.Set(request.ParamUserID, fmt.Sprintf("synth-user-%06d", rng.Intn(loadUserPool)))
		default:
			if uid := persona.RequestUserID(rng); uid != "" {
				params.Set(request.ParamUserID, uid)
			} else {
				params.Set(request.ParamUserID, fmt.Sprintf("pub-user-%08x", rng.Uint32()))
			}
		}
	}
	switch ch {
	case request.Video:
		if pod > 1 {
			params.Set("pod", fmt.Sprintf("%d", pod))
		}
		return serveVAST(client, u.PubAd+routes.PublisherAdServeVAST+"?"+params.Encode(), traceparent, p, rng)
	case request.Audio:
		return serveVAST(client, u.PubAd+routes.PublisherAdServeAudio+"?"+params.Encode(), traceparent, p, rng)
	case request.Native:
		return serveNative(client, u.PubAd+routes.PublisherAdServeNative+"?"+params.Encode(), traceparent, p, rng)
	default: // display
		return serveDisplay(client, u.SSP+routes.SSPServe+"?"+params.Encode(), traceparent, p, rng)
	}
}

// sspServeResp is the subset of the SSP's /v1/ssp/serve JSON the CLI fires. The
// URLs are the real HMAC-signed beacons the ad server built for this render.
type sspServeResp struct {
	NoBid          bool   `json:"nobid"`
	ImpressionURL  string `json:"impression_url"`
	ClickURL       string `json:"click_url"`
	ViewabilityURL string `json:"viewability_url"`
}

// serveDisplay mirrors the web display tab: GET the SSP serve endpoint, then
// fire the returned impression (always), viewability (stochastic) and click
// (stochastic) URLs — the same pixels the browser fires from the rendered ad.
func serveDisplay(client *http.Client, url, traceparent string, p profile, rng *rand.Rand) (bool, error) {
	body, err := getBody(client, url, traceparent)
	if err != nil {
		return false, err
	}
	var r sspServeResp
	if err := json.Unmarshal(body, &r); err != nil {
		return false, fmt.Errorf("decode ssp serve: %w", err)
	}
	if r.NoBid {
		return false, nil
	}
	fireGet(client, r.ImpressionURL, traceparent)
	if r.ViewabilityURL != "" && rng.Intn(100) < p.ViewPct {
		fireGet(client, r.ViewabilityURL, traceparent)
	}
	if r.ClickURL != "" && rng.Float64() < p.ClickRate {
		fireGet(client, r.ClickURL, traceparent)
		maybeConvert(client, r.ImpressionURL, traceparent, p, rng) // post-click conversion
	}
	return true, nil
}

// maybeConvert plays the advertiser's site conversion pixel for a fraction
// (p.ConvRate) of the clicks. Real conversions happen off-platform — a human
// completes an action on the advertiser's own site — so with no user in the
// loop we synthesise one: derive the trace (and campaign) from the impression
// beacon the server issued, then fire /v1/t/conv — SIGNED like every other
// beacon (243 unsigned conversions were flagged invalid in the 2026-07-19
// run; only warn-mode validation let them through).
func maybeConvert(client *http.Client, impBeacon, traceparent string, p profile, rng *rand.Rand) {
	if p.ConvRate <= 0 || rng.Float64() >= p.ConvRate {
		return
	}
	u, err := url.Parse(strings.TrimSpace(impBeacon))
	if err != nil {
		return
	}
	q := u.Query()
	if q.Get("tid") == "" {
		return // no trace → can't attribute
	}
	nq := url.Values{}
	nq.Set("tid", q.Get("tid"))
	// Carry the attribution context the impression already has.
	for _, k := range []string{"cid", "crid", "pid", "advid", "pubid"} {
		if v := q.Get(k); v != "" {
			nq.Set(k, v)
		}
	}
	nq.Set("type", "purchase")
	nq.Set("rev", fmt.Sprintf("%.2f", 5+rng.Float64()*95)) // $5–100 order value
	nq.Set("cur", "USD")
	u.Path = routes.TrackerConversion
	u.RawQuery = nq.Encode()
	// A conversion is validated PER-ADVERTISER under the prod-shaped
	// tracker.conversion_strict_advertiser_key (G7): sign with this advertiser's
	// own deterministic dev key (the seed minted the matching hmac_conversion
	// secret). advid="" → DevConversionKey returns the platform key.
	fireGet(client, adserving.SignURL(u.String(), adserving.DevConversionKey(nq.Get("advid"))), traceparent)
}

// playbackErrors64 counts simulated VAST playback errors (profile.ErrorRate
// rolls) so the run summary can explain the missing completes.
var playbackErrors64 int64

// serveVAST mirrors the web video/audio tabs: GET the VAST, then fire the
// Impression + quartile Tracking URLs the way a spec-compliant player does as
// it plays through the ad, plus a stochastic click. These are the server's
// signed beacons. Player-side IAB bracket macros ([CACHEBUSTING], [TIMESTAMP],
// [ADPLAYHEAD], [ERRORCODE]) are substituted before every fire — the server
// emits them as literal tokens outside the HMAC. A profile.ErrorRate roll
// simulates a fatal playback failure: impression + start, then the <Error>
// URIs with code 405 (problem displaying MediaFile), then stop.
func serveVAST(client *http.Client, url, traceparent string, p profile, rng *rand.Rand) (bool, error) {
	body, err := getBody(client, url, traceparent)
	if err != nil {
		return false, err
	}
	var doc vast.VAST
	if err := xml.Unmarshal(body, &doc); err != nil {
		return false, fmt.Errorf("parse vast: %w", err)
	}
	if len(doc.Ads) == 0 {
		return false, nil
	}
	for _, ad := range doc.Ads {
		if ad.InLine == nil {
			continue
		}
		// Player-side macro substitution at the playhead position `pos`.
		expand := func(uri string, v vast.URIMacroValues) string {
			v.Rand = rng.Int63
			return vast.ExpandURIMacros(strings.TrimSpace(uri), v)
		}
		var firstImp string
		for _, imp := range ad.InLine.Impressions {
			uri := expand(imp.URI, vast.URIMacroValues{})
			if firstImp == "" {
				firstImp = uri
			}
			fireGet(client, uri, traceparent)
		}
		for _, cr := range ad.InLine.Creatives.Creatives {
			if cr.Linear == nil {
				continue
			}
			adDur := time.Duration(cr.Linear.Duration)
			fireEvent := func(ev string, pos time.Duration) {
				if cr.Linear.TrackingEvents == nil {
					return
				}
				for _, tr := range cr.Linear.TrackingEvents.Tracking {
					if tr.Event == ev {
						fireGet(client, expand(tr.URI, vast.URIMacroValues{AdPlayhead: pos, ContentPlayhead: pos}), traceparent)
					}
				}
			}
			if p.ErrorRate > 0 && rng.Float64() < p.ErrorRate {
				// Fatal mid-start failure: a spec player fires what it saw
				// (impression already sent + start), pings every <Error> URI
				// with [ERRORCODE] substituted, and abandons the ad.
				fireEvent("start", 0)
				for _, e := range ad.InLine.Errors {
					fireGet(client, expand(e.URI, vast.URIMacroValues{ErrorCode: 405}), traceparent)
				}
				atomic.AddInt64(&playbackErrors64, 1)
				continue
			}
			// Standard playback progression at quartile playheads.
			fireEvent("start", 0)
			fireEvent("firstQuartile", adDur/4)
			fireEvent("midpoint", adDur/2)
			fireEvent("thirdQuartile", 3*adDur/4)
			fireEvent("complete", adDur)
			if cr.Linear.VideoClicks != nil && rng.Float64() < p.ClickRate {
				if ct := cr.Linear.VideoClicks.ClickThrough; ct != nil {
					fireGet(client, expand(ct.URI, vast.URIMacroValues{AdPlayhead: adDur / 2}), traceparent)
				}
				for _, c := range cr.Linear.VideoClicks.ClickTracking {
					fireGet(client, expand(c.URI, vast.URIMacroValues{AdPlayhead: adDur / 2}), traceparent)
				}
				maybeConvert(client, firstImp, traceparent, p, rng) // post-click conversion
			}
		}
	}
	return true, nil
}

var trackerURLRe = regexp.MustCompile(`(?:src|href)="([^"]+/v1/t/[^"]+)"`)

// serveNative mirrors the web native tab: GET the native HTML fragment and fire
// the tracker pixels/links embedded in it (impression always, click stochastic)
// — the same beacons the browser fires when it renders the card.
func serveNative(client *http.Client, url, traceparent string, p profile, rng *rand.Rand) (bool, error) {
	body, err := getBody(client, url, traceparent)
	if err != nil {
		return false, err
	}
	served := false
	var impBeacon string
	for _, m := range trackerURLRe.FindAllStringSubmatch(string(body), -1) {
		beacon := html.UnescapeString(m[1]) // HTML attrs escape & as &amp;
		switch {
		case strings.Contains(beacon, routes.TrackerImpression):
			fireGet(client, beacon, traceparent)
			served = true
			impBeacon = beacon
		case strings.Contains(beacon, routes.TrackerClick):
			if rng.Float64() < p.ClickRate {
				fireGet(client, beacon, traceparent)
				maybeConvert(client, impBeacon, traceparent, p, rng) // post-click conversion
			}
		}
	}
	return served, nil
}

func getBody(client *http.Client, url, traceparent string) ([]byte, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("traceparent", traceparent)
	// Browser-shaped client so downstream fraud checks don't drop us.
	req.Header.Set("User-Agent", "Mozilla/5.0 (adtech-simulator)")
	req.Header.Set("Referer", "https://simulator.dev/")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// 204 No Content is an honest no-fill (the publisher-adserver returns it
	// for a genuine native no-bid with the demo stub off). Not an error —
	// return an empty body so callers see "no ad" and record a no-fill.
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}
