//go:build e2e

package harness

import (
	"context"
	"encoding/xml"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/simulator/pages"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/vast"
)

// VisitPage replays an external-publisher page WITHOUT a browser. It reads the
// same pages.Layout the demosite renders, and for every ad slot it:
//
//  1. serves the ad via the format's real /v1/pubad/* endpoint,
//  2. extracts the SIGNED impression URL (+ viewability URL for display/video)
//     the ad server minted — exactly the URLs the browser would have loaded,
//  3. fires them (impression pixel, then a measured viewability beacon).
//
// It returns a per-slot result so the test can assert that every slot's trace
// produced exactly one impression (and, where applicable, one viewable view) in
// ClickHouse — i.e. an N-ad page lands N impressions downstream, zero slippage.
//
// This is the non-browser twin of a real visit: the ONLY thing it doesn't do is
// run the page's JavaScript. Because both sides import pkg/simulator/pages, the
// slot count and formats it fires are the same the live page requests.
func (h *Harness) VisitPage(t *testing.T, slug string) PageVisitResult {
	t.Helper()
	layout, ok := pages.BySlug(slug)
	if !ok {
		t.Fatalf("VisitPage: unknown layout %q", slug)
	}
	// Window start: a moment before the first serve, for time-scoped queries.
	from := time.Now().Add(-2 * time.Second).UTC()
	res := PageVisitResult{Slug: slug, Layout: layout, From: from}
	for _, slot := range layout.Slots {
		res.Slots = append(res.Slots, h.visitSlot(t, slot))
	}
	return res
}

// PageVisitResult is the outcome of a VisitPage — one SlotVisit per ad slot,
// in page order, plus the window start for time-scoped ClickHouse queries.
type PageVisitResult struct {
	Slug   string
	Layout pages.Layout
	From   time.Time
	Slots  []SlotVisit
}

// SlotVisit records what one slot did: whether it filled, the trace the served
// ad carried, whether a viewability beacon was fired, and the tracker's verdict.
type SlotVisit struct {
	Format    pages.Format
	Label     string
	Filled    bool
	TraceID   string
	FiredView bool // a viewability beacon was fired (display/video only)
	Viewable  bool // the tracker judged the view IAB-viewable
}

// FilledTraces returns the trace IDs of every slot that filled — the set of
// impressions the visit must have landed in ClickHouse.
func (r PageVisitResult) FilledTraces() []string {
	var out []string
	for _, s := range r.Slots {
		if s.Filled && s.TraceID != "" {
			out = append(out, s.TraceID)
		}
	}
	return out
}

// visitSlot serves one slot by format and fires its beacons. Every format reduces
// to the same shape: a signed impression URL (fired as-is) and, for viewable
// formats, a signed viewability base to which the client appends its measurement.
func (h *Harness) visitSlot(t *testing.T, slot pages.Slot) SlotVisit {
	t.Helper()
	sv := SlotVisit{Format: slot.Format, Label: slot.Label}

	var impURL, viewURL string
	switch slot.Format {
	case pages.Display:
		resp := h.ServePubAdRaw(t, "placement_id="+slot.PlacementKey+"&geo=USA&device=desktop")
		if resp.NoBid || resp.ImpressionURL == "" {
			return sv
		}
		impURL, viewURL = resp.ImpressionURL, resp.ViewabilityURL
	case pages.Native:
		// Native returns an HTML fragment with the impression pixel embedded as an
		// <img>. Pull the /v1/t/imp URL out of the markup (browser fires it on
		// render). Native has no viewability beacon.
		htmlBody := h.getBody(t, h.URLs.PublisherAdServer+routes.PublisherAdServeNative+"?placement_id="+slot.PlacementKey+"&geo=USA&device=desktop")
		impURL = extractPixelURL(htmlBody)
		if impURL == "" {
			return sv
		}
	case pages.Video:
		xmlBody := h.getBody(t, h.URLs.PublisherAdServer+routes.PublisherAdServeVAST+"?placement_id="+slot.PlacementKey+"&geo=USA&device=desktop")
		impURL, viewURL = extractVASTURLs(xmlBody)
		if impURL == "" {
			return sv
		}
	case pages.Audio:
		xmlBody := h.getBody(t, h.URLs.PublisherAdServer+routes.PublisherAdServeAudio+"?placement_id="+slot.PlacementKey+"&geo=USA&device=desktop")
		impURL, _ = extractVASTURLs(xmlBody) // audio isn't viewable
		if impURL == "" {
			return sv
		}
	default:
		t.Fatalf("visitSlot: unhandled format %q", slot.Format)
	}

	sv.Filled = true
	sv.TraceID = traceIDFromURL(impURL)

	// Fire the impression pixel (already signed by the ad server — fire as-is).
	h.FireImpressionURL(t, impURL)

	// Fire a measured viewability beacon where the format supports it. dur/pct
	// clear BOTH the display (1s/50%) and video (2s/50%) IAB thresholds.
	if viewURL != "" {
		sv.FiredView = true
		sv.Viewable = h.FireViewURL(t, viewURL+"&dur=2500&pct=80&area=60000")
	}
	return sv
}

// getBody GETs a URL with a browser-shaped UA/Referer (so the fraud filter
// doesn't drop the eventual beacons) and returns the body. Non-2xx is fatal.
func (h *Harness) getBody(t *testing.T, u string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		t.Fatalf("build serve request: %v", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (e2e-harness)")
	req.Header.Set("Referer", "https://e2e.test/")
	resp, err := h.HTTP.Do(req)
	if err != nil {
		t.Fatalf("serve call %s: %v", u, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	// 204 = honest no-bid (native); return empty so the slot records unfilled.
	if resp.StatusCode == http.StatusNoContent {
		return ""
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("serve %s status %d: %s", u, resp.StatusCode, string(body))
	}
	return string(body)
}

var pixelRe = regexp.MustCompile(`src="([^"]*/v1/t/imp[^"]*)"`)

// extractPixelURL pulls the impression pixel URL out of a native HTML fragment
// and un-escapes HTML entities (the template escapes & as &amp;).
func extractPixelURL(htmlBody string) string {
	m := pixelRe.FindStringSubmatch(htmlBody)
	if len(m) < 2 {
		return ""
	}
	return html.UnescapeString(m[1])
}

// extractVASTURLs parses a VAST document and returns (impressionURL,
// viewabilityURL). The viewability URL is the non-standard event="viewable"
// tracker our player self-fires; it's absent for audio.
func extractVASTURLs(xmlBody string) (imp, view string) {
	var doc vast.VAST
	if err := xml.Unmarshal([]byte(xmlBody), &doc); err != nil {
		return "", ""
	}
	for _, ad := range doc.Ads {
		if ad.InLine == nil {
			continue
		}
		if len(ad.InLine.Impressions) > 0 {
			imp = ad.InLine.Impressions[0].URI
		}
		for _, cr := range ad.InLine.Creatives.Creatives {
			if cr.Linear == nil || cr.Linear.TrackingEvents == nil {
				continue
			}
			for _, tr := range cr.Linear.TrackingEvents.Tracking {
				if tr.Event == "viewable" {
					view = tr.URI
				}
			}
		}
	}
	return imp, view
}

// ImpressionsByTrace returns how many impression rows in ClickHouse carry this
// trace — the per-slot anti-slippage assertion (a served ad → exactly one).
func (h *Harness) ImpressionsByTrace(t *testing.T, traceID string) int {
	t.Helper()
	return h.ClickHouseScalar(t, "SELECT count() FROM adtech.impressions WHERE trace_id = '"+traceID+"'")
}

// ViewableViewsByTrace returns how many IAB-viewable view rows in ClickHouse
// carry this trace — proves a viewability beacon reached the analytical store.
func (h *Harness) ViewableViewsByTrace(t *testing.T, traceID string) int {
	t.Helper()
	return h.ClickHouseScalar(t, "SELECT count() FROM adtech.views WHERE trace_id = '"+traceID+"' AND iab_viewable = 1")
}

// traceIDFromURL reads the tid query param — every beacon the ad server mints
// carries the auction's trace, so this is the slot's trace regardless of format.
func traceIDFromURL(beaconURL string) string {
	u, err := url.Parse(beaconURL)
	if err != nil {
		return ""
	}
	return u.Query().Get("tid")
}
