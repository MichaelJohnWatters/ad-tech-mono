//go:build e2e

// VAST 4.2 bracket macros + [ERRORCODE] capture, end-to-end against the live
// stack: the served VAST carries the IAB bracket-macro params as raw literals
// OUTSIDE the HMAC (ec/cb/ts/pos appended after sig=); a player-substituted
// error beacon lands in media_events with the real error_code; junk codes are
// clamped to 0; an unsubstituted literal macro records as 900 (spec rule);
// and the unsigned-param filter does NOT weaken the signature gate — a
// tampered SIGNED param still 403s under strict validation.
package e2e

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/vast"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestVideoVASTErrorCodes(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.Reset(t)

	uniq := fmt.Sprintf("vec-%d", time.Now().UnixNano())

	// Own video inventory so the fill path actually runs (video_vast_test shape).
	pubAcc := h.CreatePublisher(t, "vec-pub-"+uniq)
	pub := h.AddPublisher(t, pubAcc, "vec-pub-"+uniq, "vec-"+uniq+".test")
	h.AddVideoPlacement(t, pub, "vec-pl-"+uniq, 1.00, 6, 30)
	adv := h.CreateAdvertiser(t, "adv-acme")
	h.GrantBalance(t, adv.ID, 100_000, "vec-grant-"+uniq)
	vio := h.CreateInsertionOrder(t, adv, "vec-io-"+uniq, 5000)
	h.CreateVideoCampaign(t, adv, vio, "vec-li-"+uniq, 10.0, 500,
		"vec-cr-"+uniq, "ford-"+uniq+".test", 15, harness.Targeting{})
	h.RefreshAllCaches(t)

	// fetchFilledVAST returns a freshly-auctioned VAST document — each call is
	// a new trace, which the per-trace "video:error" dedup makes necessary for
	// every error-firing scenario below.
	fetchFilledVAST := func(t *testing.T) *vast.VAST {
		t.Helper()
		var doc vast.VAST
		harness.WaitFor(t, 20*time.Second, "video auction fills the VAST", func() bool {
			reqURL := h.URLs.PublisherAdServer + routes.PublisherAdServeVAST + "?placement_id=vec-pl-" + uniq
			req, _ := http.NewRequest(http.MethodGet, reqURL, nil)
			resp, err := h.HTTP.Do(req)
			if err != nil {
				return false
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return false
			}
			doc = vast.VAST{}
			if err := xml.Unmarshal(body, &doc); err != nil {
				return false
			}
			return len(doc.Ads) > 0 && doc.Ads[0].InLine != nil
		})
		return &doc
	}

	// fire GETs a beacon with player headers (the fraud gate drops bot UAs).
	fire := func(t *testing.T, beacon string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, beacon, nil)
		if err != nil {
			t.Fatalf("build beacon request %q: %v", beacon, err)
		}
		browserHeaders(req)
		resp, err := h.HTTP.Do(req)
		if err != nil {
			t.Fatalf("fire beacon: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	errorURI := func(t *testing.T, doc *vast.VAST) (uri, tid string) {
		t.Helper()
		if len(doc.Ads[0].InLine.Errors) == 0 {
			t.Fatal("filled VAST has no <Error> element")
		}
		uri = strings.TrimSpace(doc.Ads[0].InLine.Errors[0].URI)
		u, err := url.Parse(strings.ReplaceAll(uri, "[", "%5B")) // tolerate raw brackets for parsing only
		if err != nil {
			t.Fatalf("parse error URI %q: %v", uri, err)
		}
		tid = u.Query().Get("tid")
		if tid == "" {
			t.Fatalf("error URI carries no tid: %s", uri)
		}
		return uri, tid
	}

	// --- 1. The emitted document carries the bracket macros as raw literals,
	// appended AFTER the signature (unsigned, player-substituted).
	doc := fetchFilledVAST(t)
	errURI, tid := errorURI(t, doc)
	for _, want := range []string{"ec=[ERRORCODE]", "cb=[CACHEBUSTING]", "ts=[TIMESTAMP]", "pos=[ADPLAYHEAD]"} {
		if !strings.Contains(errURI, want) {
			t.Errorf("<Error> URI missing raw literal %q: %s", want, errURI)
		}
	}
	if sigIdx, cbIdx := strings.Index(errURI, "sig="), strings.Index(errURI, "cb="); sigIdx == -1 || cbIdx < sigIdx {
		t.Errorf("macro params must ride AFTER sig= (unsigned): %s", errURI)
	}
	var startURI string
	for _, cr := range doc.Ads[0].InLine.Creatives.Creatives {
		if cr.Linear == nil || cr.Linear.TrackingEvents == nil {
			continue
		}
		for _, tr := range cr.Linear.TrackingEvents.Tracking {
			if tr.Event == "start" {
				startURI = strings.TrimSpace(tr.URI)
			}
			// "viewable" is the IAB viewability beacon (/v1/t/view) with its own
			// client-measured dur/pct/area contract — no bracket macros there.
			if tr.Event == "error" || tr.Event == "viewable" {
				continue
			}
			if !strings.Contains(tr.URI, "cb=[CACHEBUSTING]") {
				t.Errorf("tracking %q missing cb=[CACHEBUSTING]: %s", tr.Event, tr.URI)
			}
			if strings.Contains(tr.URI, "ec=[ERRORCODE]") {
				t.Errorf("non-error tracking %q must not carry ec=: %s", tr.Event, tr.URI)
			}
		}
	}
	if startURI == "" {
		t.Fatal("filled VAST has no start tracking beacon")
	}

	// --- 2. Player substitutes [ERRORCODE]=405 and fires → recorded with the code.
	substituted := vast.ExpandURIMacros(errURI, vast.URIMacroValues{
		ErrorCode:  405,
		AdPlayhead: 3 * time.Second,
	})
	if code := fire(t, substituted); code != http.StatusNoContent {
		t.Fatalf("substituted error beacon status: got %d, want 204 (url=%s)", code, substituted)
	}
	harness.WaitFor(t, 15*time.Second, "error_code=405 recorded", func() bool {
		return h.MediaEventsByTrace(t, tid, "video", "error", "405") >= 1
	})

	// --- 3. Negative: a junk code is clamped to 0, never stored verbatim.
	t.Run("junk_error_code_clamped", func(t *testing.T) {
		doc := fetchFilledVAST(t)
		errURI, tid := errorURI(t, doc)
		junk := vast.ExpandURIMacros(strings.Replace(errURI, "ec=[ERRORCODE]", "ec=31337", 1), vast.URIMacroValues{})
		if code := fire(t, junk); code != http.StatusNoContent {
			t.Fatalf("junk-code beacon status: got %d, want 204", code)
		}
		harness.WaitFor(t, 15*time.Second, "junk code recorded as 0", func() bool {
			return h.MediaEventsByTrace(t, tid, "video", "error", "0") >= 1
		})
		if n := h.MediaEventsByTrace(t, tid, "video", "error", "31337"); n != 0 {
			t.Errorf("junk code 31337 must never be stored, found %d rows", n)
		}
	})

	// --- 4. A macro-incapable player leaves [ERRORCODE] literal → recorded as
	// 900 (VAST 4.2: unsubstituted macro = undefined error).
	t.Run("literal_macro_records_900", func(t *testing.T) {
		doc := fetchFilledVAST(t)
		errURI, tid := errorURI(t, doc)
		// Substitute cb/ts/pos but leave ec=[ERRORCODE] in place (url-encoded
		// so the raw brackets survive the HTTP client's URL parser).
		partial := vast.ExpandURIMacros(strings.Replace(errURI, "ec=[ERRORCODE]", "ec=%5BERRORCODE%5D", 1), vast.URIMacroValues{})
		if code := fire(t, partial); code != http.StatusNoContent {
			t.Fatalf("literal-macro beacon status: got %d, want 204", code)
		}
		harness.WaitFor(t, 15*time.Second, "literal macro recorded as 900", func() bool {
			return h.MediaEventsByTrace(t, tid, "video", "error", "900") >= 1
		})
	})

	// --- 5. Negative: the unsigned-param filter must NOT weaken the HMAC gate.
	// Tamper a SIGNED param (event) while keeping the macro params appended —
	// under strict validation the tracker 403s.
	t.Run("tampered_signed_param_still_403s", func(t *testing.T) {
		const pod = "tracker-0"
		const key = "tracker.signature_validation"
		// Deployed baseline is strict already; force it so the assertion is
		// unambiguous, and DELETE the override after (reverts to deployed).
		t.Cleanup(func() { h.DeleteConfig(t, key) })
		h.SetConfigForPod(t, key, "true", pod)

		harness.WaitFor(t, 35*time.Second, "tampered quartile rejected 403", func() bool {
			doc := fetchFilledVAST(t)
			var start string
			for _, cr := range doc.Ads[0].InLine.Creatives.Creatives {
				if cr.Linear != nil && cr.Linear.TrackingEvents != nil {
					for _, tr := range cr.Linear.TrackingEvents.Tracking {
						if tr.Event == "start" {
							start = strings.TrimSpace(tr.URI)
						}
					}
				}
			}
			if start == "" {
				return false
			}
			tampered := vast.ExpandURIMacros(strings.Replace(start, "event=start", "event=complete", 1), vast.URIMacroValues{})
			return fire(t, tampered) == http.StatusForbidden
		})
	})
}
