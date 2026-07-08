// cmd/ssai is the server-side ad insertion (SSAI) stitcher. It sits between a
// video player and the origin content: the player fetches an HLS manifest from
// here instead of the origin, and the stitcher rewrites it so ad segments are
// spliced directly into the content stream at each ad break. Because the ads
// are part of the stream the player requests segment-by-segment, ad blockers
// can't strip them — the SSAI advantage over client-side VAST.
//
// Per request the stitcher:
//  1. Serves (or fetches) the origin content manifest with #EXT-X-CUE-OUT /
//     #EXT-X-CUE-IN ad-break markers.
//  2. For each break, runs a real auction via the SSP (channel=video),
//     forwarding the viewer's geo/device/consent/identity signals.
//  3. Slices the winning ad into segments and splices them over the content-
//     during-break segments (pkg/ssai).
//  4. Fires the impression beacon server-side immediately, and points each ad
//     segment at /v1/ssai/seg, which fires that segment's quartile beacon
//     server-side when the player fetches it, then redirects to the media.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/health"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ssai"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
)

var log = logger.New(constants.ServiceSSAI)

// sampleContent is the origin VOD manifest the demo stitches. Two ad breaks
// (pre-roll at the top, mid-roll after 4 content segments) marked with the
// standard CUE-OUT/CUE-IN tags. Real deployments would proxy the publisher's
// origin manifest instead.
const sampleContent = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:6
#EXT-X-MEDIA-SEQUENCE:0
#EXT-X-PLAYLIST-TYPE:VOD
#EXT-X-CUE-OUT:DURATION=30
#EXTINF:6.0,
content_000.ts
#EXT-X-CUE-IN
#EXTINF:6.0,
content_001.ts
#EXTINF:6.0,
content_002.ts
#EXTINF:6.0,
content_003.ts
#EXTINF:6.0,
content_004.ts
#EXT-X-CUE-OUT:DURATION=15
#EXTINF:6.0,
content_005.ts
#EXT-X-CUE-IN
#EXTINF:6.0,
content_006.ts
#EXTINF:6.0,
content_007.ts
#EXT-X-ENDLIST
`

func main() {
	sc := config.Setup(constants.ServiceSSAI, ssaiSchema, log)
	cfg := sc.Cfg
	hlth := health.New()
	lc := lifecycle.New(log)

	port := cfg.Get("ssai.port", routes.PortSSAI)
	sspURL := cfg.Get("ssai.ssp_url", routes.DefaultSSPURL)
	trackerURL := cfg.Get("ssai.tracker_url", routes.DefaultTrackerURL)
	publicURL := cfg.Get("ssai.public_url", routes.DefaultGatewayURL)

	otelShutdown := tracing.Init(context.Background(), tracing.Config{
		ServiceName:    constants.ServiceSSAI,
		ServiceVersion: cfg.Get("otel.service_version", "dev"),
		Endpoint:       cfg.Get("otel.endpoint", "localhost:4318"),
		SampleRatio:    cfg.GetFloat("otel.sample_ratio", 1.0),
		Log:            log,
	})
	lc.OnShutdown("otel", func(ctx context.Context) error { return otelShutdown(ctx) })

	deps := &stitcherDeps{
		sspURL:     sspURL,
		trackerURL: trackerURL,
		publicURL:  publicURL,
		segDurFn:   func() float64 { return cfg.GetFloat("ssai.segment_duration", 6) },
		placementFn: func() string {
			return cfg.Get("ssai.ad_placement_id", "pl-sim-video")
		},
		client: &http.Client{Timeout: 4 * time.Second},
	}

	metrics := middleware.NewMetrics(constants.ServiceSSAI)
	mux := http.NewServeMux()
	mux.Handle(routes.Healthz, hlth.LivenessHandler())
	mux.Handle(routes.Readyz, hlth.ReadinessHandler())
	mux.HandleFunc(routes.SSAIContent, serveContentManifest)
	mux.HandleFunc(routes.SSAIManifest, deps.manifestHandler)
	mux.HandleFunc(routes.SSAISegment, deps.segmentHandler)

	handler := tracing.HTTPMiddleware(constants.ServiceSSAI)(metrics.Wrap(middleware.CORS(mux)))
	server := &http.Server{Addr: ":" + port, Handler: handler, ReadTimeout: 5 * time.Second, WriteTimeout: 15 * time.Second}

	log.Info("ssai stitcher starting", "port", port, "ssp", sspURL, "tracker", trackerURL)
	lifecycle.ServeHTTP(lc, server, log, 30*time.Second)
}

type stitcherDeps struct {
	sspURL      string
	trackerURL  string
	publicURL   string
	segDurFn    func() float64
	placementFn func() string
	client      *http.Client
}

// serveContentManifest returns the sample origin content manifest (with ad-break
// markers) so the stitcher has something to rewrite in the demo.
func serveContentManifest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, sampleContent)
}

// sspWinner is the subset of the SSP channel=video response the stitcher needs.
type sspWinner struct {
	TraceID          string  `json:"trace_id"`
	NoBid            bool    `json:"nobid"`
	CreativeID       string  `json:"creative_id"`
	CampaignID       string  `json:"campaign_id"`
	PlacementID      string  `json:"placement_id"`
	PublisherID      string  `json:"publisher_id"`
	AdvertiserID     string  `json:"advertiser_id"`
	AdvertiserDomain string  `json:"advertiser_domain"`
	ClearingPrice    float64 `json:"clearing_price"`
	Currency         string  `json:"currency"`
	DurationSeconds  int     `json:"duration_seconds"`
	MediaURL         string  `json:"media_url"`
}

// manifestHandler is the player-facing endpoint. It parses the content
// manifest, runs a per-break auction, stitches the winning ads in, fires the
// impression beacon server-side, and returns the rewritten manifest.
func (d *stitcherDeps) manifestHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	traceID := tracing.TraceIDFromContext(ctx)
	reqLog := logger.WithContext(log, logger.WithTraceID(ctx, traceID))

	m, err := ssai.ParseMedia(sampleContent)
	if err != nil {
		http.Error(w, "content manifest parse failed", http.StatusInternalServerError)
		return
	}

	// Session id lets the segment beacons correlate back to this stitch. Derive
	// from the trace so all beacons share the auction trace where possible.
	session := traceID
	if session == "" {
		session = fmt.Sprintf("ssai-%d", time.Now().UnixMilli())
	}

	segDur := d.segDurFn()
	filled := 0
	m.Stitch(func(i int, span ssai.BreakSpan) []ssai.Segment {
		winner := d.runAuction(ctx, r, span.Duration, reqLog)
		if winner == nil || winner.NoBid || winner.MediaURL == "" {
			reqLog.Info("ssai break unfilled, keeping content", "break", i)
			return nil // slate / passthrough
		}
		filled++
		adTrace := winner.TraceID
		if adTrace == "" {
			adTrace = session
		}
		// Impression fires server-side now: the ad is guaranteed to be in the
		// stream, so this is the SSAI impression moment.
		d.fireBeacon(ctx, d.impressionURL(adTrace, winner))

		dur := float64(winner.DurationSeconds)
		if dur <= 0 {
			dur = span.Duration
		}
		adIdx := i
		return ssai.SegmentAds(dur, segDur, func(n int, start float64) string {
			ev := quartileForSegment(start, dur, segDur)
			return d.segmentURL(session, adTrace, adIdx, n, ev, winner.MediaURL)
		})
	})

	reqLog.Info("ssai manifest stitched",
		"breaks", len(m.Breaks()), "filled", filled, "ad_seconds", m.AdDuration())

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, m.Render())
}

// runAuction calls the SSP channel=video, forwarding the viewer's signals so
// the ad is targeted to the real request (not a hardcoded default).
func (d *stitcherDeps) runAuction(ctx context.Context, r *http.Request, breakDur float64, reqLog *slog.Logger) *sspWinner {
	q := url.Values{}
	for k, v := range r.URL.Query() {
		q[k] = append([]string(nil), v...)
	}
	q.Set("channel", "video")
	if q.Get("placement_id") == "" {
		q.Set("placement_id", d.placementFn())
	}
	if q.Get("geo") == "" {
		q.Set("geo", "USA")
	}
	if q.Get("device") == "" {
		q.Set("device", "ctv") // SSAI is overwhelmingly CTV/OTT
	}

	target := d.sspURL + routes.SSPServe + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		reqLog.Warn("ssai auction request build failed", "error", err)
		return nil
	}
	tracing.InjectHTTP(ctx, req)
	resp, err := d.client.Do(req)
	if err != nil {
		reqLog.Warn("ssai auction call failed", "error", err)
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		reqLog.Warn("ssai auction non-200", "status", resp.StatusCode)
		return nil
	}
	var winner sspWinner
	if err := json.Unmarshal(body, &winner); err != nil {
		reqLog.Warn("ssai auction decode failed", "error", err)
		return nil
	}
	return &winner
}

// segmentHandler is the per-ad-segment beacon endpoint referenced by the
// stitched manifest. When the player fetches an ad segment, this fires that
// segment's quartile beacon server-side, then 302-redirects to the real media
// so playback proceeds. This is the SSAI server-side beacon model: the player
// never fires ad beacons itself (it doesn't know these are ads).
func (d *stitcherDeps) segmentHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redir := q.Get("redir")
	event := q.Get("event")
	tid := q.Get("ad")
	if event != "" && tid != "" {
		d.fireBeacon(r.Context(), fmt.Sprintf("%s/v1/t/video?tid=%s&event=%s&sig=ssai",
			d.trackerURL, url.QueryEscape(tid), url.QueryEscape(event)))
	}
	if redir == "" {
		http.Error(w, "missing redir", http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, redir, http.StatusFound)
}

func (d *stitcherDeps) impressionURL(tid string, wn *sspWinner) string {
	return fmt.Sprintf("%s/v1/t/imp?tid=%s&cid=%s&crid=%s&pid=%s&pubid=%s&price=%s&cur=%s&sig=ssai",
		d.trackerURL, url.QueryEscape(tid), url.QueryEscape(wn.CampaignID), url.QueryEscape(wn.CreativeID),
		url.QueryEscape(wn.PlacementID), url.QueryEscape(wn.PublisherID),
		strconv.FormatFloat(wn.ClearingPrice, 'f', 4, 64), url.QueryEscape(defaultStr(wn.Currency, "USD")))
}

// segmentURL builds the manifest URI for one ad segment: a call back into this
// service's segment beacon endpoint (via the browser-reachable public URL),
// carrying the quartile event to fire and the real media to redirect to.
func (d *stitcherDeps) segmentURL(session, adTrace string, adIdx, n int, event, mediaURL string) string {
	q := url.Values{}
	q.Set("session", session)
	q.Set("ad", adTrace)
	q.Set("break", strconv.Itoa(adIdx))
	q.Set("seg", strconv.Itoa(n))
	if event != "" {
		q.Set("event", event)
	}
	q.Set("redir", mediaURL)
	return d.publicURL + routes.SSAISegment + "?" + q.Encode()
}

// fireBeacon fires a tracker beacon server-side with a browser-shaped UA so the
// tracker's fraud check doesn't drop it as a bot (matches the simulator's
// pixel-firing). Fire-and-forget.
func (d *stitcherDeps) fireBeacon(ctx context.Context, beaconURL string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, beaconURL, nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (adtech-ssai)")
	req.Header.Set("Referer", "https://ssai.adtech.local/")
	tracing.InjectHTTP(ctx, req)
	if resp, err := d.client.Do(req); err == nil {
		resp.Body.Close()
	}
}

// quartileForSegment maps an ad segment (by its start offset) to the VAST
// quartile event it represents. Segment 0 fires "start"; segments crossing the
// 25/50/75% marks fire the quartile; the final segment fires "complete".
func quartileForSegment(start, total, segDur float64) string {
	if start <= 0.001 {
		return "start"
	}
	if start+segDur >= total-0.001 {
		return "complete"
	}
	frac := start / total
	switch {
	case frac >= 0.75:
		return "thirdQuartile"
	case frac >= 0.5:
		return "midpoint"
	case frac >= 0.25:
		return "firstQuartile"
	default:
		return "start"
	}
}

func defaultStr(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
