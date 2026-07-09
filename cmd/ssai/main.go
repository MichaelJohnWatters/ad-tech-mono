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
	"bytes"
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

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/health"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ssai"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/transcode"
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

// sampleAudioContent is the built-in audio origin (podcast / streaming-radio
// style): AAC segments in an HLS media playlist with one mid-roll avail. Served
// for ?channel=audio when no real origin is configured. Real deployments proxy
// the publisher's audio origin instead.
const sampleAudioContent = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:10
#EXT-X-MEDIA-SEQUENCE:0
#EXT-X-PLAYLIST-TYPE:VOD
#EXTINF:10.0,
audio_000.ts
#EXTINF:10.0,
audio_001.ts
#EXT-X-CUE-OUT:DURATION=30
#EXTINF:10.0,
audio_002.ts
#EXT-X-CUE-IN
#EXTINF:10.0,
audio_003.ts
#EXTINF:10.0,
audio_004.ts
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

	metrics := middleware.NewMetrics(constants.ServiceSSAI)

	deps := &stitcherDeps{
		sspURL:     sspURL,
		trackerURL: trackerURL,
		publicURL:  publicURL,
		placementFn: func() string {
			return cfg.Get("ssai.ad_placement_id", "pl-sim-video")
		},
		originFn:        func() string { return cfg.Get("ssai.origin_url", "") },
		transcoderURL:   cfg.Get("ssai.transcoder_url", routes.DefaultTranscoderURL),
		maxPodAdsFn:     func() int { return cfg.GetInt("ssai.max_pod_ads", 4) },
		slateCreativeFn: func() string { return cfg.Get("ssai.slate_creative_id", "") },
		slateMediaFn:    func() string { return cfg.Get("ssai.slate_media_url", "") },
		metrics:         newStitcherMetrics(metrics.Registry()),
		client:          &http.Client{Timeout: 4 * time.Second},
	}

	mux := http.NewServeMux()
	mux.Handle(routes.Healthz, hlth.LivenessHandler())
	mux.Handle(routes.Readyz, hlth.ReadinessHandler())
	mux.Handle(routes.Metrics, metrics.Handler())
	mux.HandleFunc(routes.SSAIContent, serveContentManifest)
	mux.HandleFunc(routes.SSAIManifest, deps.manifestHandler)
	mux.HandleFunc(routes.SSAISegment, deps.segmentHandler)

	handler := tracing.HTTPMiddleware(constants.ServiceSSAI)(metrics.Wrap(middleware.CORS(mux)))
	server := &http.Server{Addr: ":" + port, Handler: handler, ReadTimeout: 5 * time.Second, WriteTimeout: 15 * time.Second}

	log.Info("ssai stitcher starting", "port", port, "ssp", sspURL, "tracker", trackerURL)
	lifecycle.ServeHTTP(lc, server, log, 30*time.Second)
}

type stitcherDeps struct {
	sspURL          string
	trackerURL      string
	publicURL       string
	placementFn     func() string
	originFn        func() string // configured origin manifest URL ("" = built-in sample)
	transcoderURL   string        // runtime ad-conditioning service ("" = disabled)
	maxPodAdsFn     func() int    // max ads per avail (ad pod); nil/≤0 → 1
	slateCreativeFn func() string // slate creative id ("" = no slate, keep content)
	slateMediaFn    func() string // slate media URL (to warm-condition the slate)
	metrics         *stitcherMetrics
	client          *http.Client
}

func (d *stitcherDeps) maxPodAds() int {
	if d.maxPodAdsFn == nil {
		return 1
	}
	if n := d.maxPodAdsFn(); n > 0 {
		return n
	}
	return 1
}

// conditionCached asks the transcoder for the winner's already-conditioned
// segments for a profile (cache-only — never triggers a transcode, so serving
// doesn't block on ffmpeg). Returns nil when the transcoder is disabled,
// unreachable, or the ad isn't conditioned yet (the caller then slates + warms).
func (d *stitcherDeps) conditionCached(ctx context.Context, winner *sspWinner, p transcode.Profile, reqLog *slog.Logger) *transcode.Conditioned {
	if d.transcoderURL == "" || winner.CreativeID == "" {
		return nil
	}
	body, _ := json.Marshal(map[string]any{
		"creative_id": winner.CreativeID,
		"media_url":   winner.MediaURL,
		"profile":     p,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		d.transcoderURL+routes.TranscodeCondition+"?cache_only=1", bytes.NewReader(body))
	if err != nil {
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	tracing.InjectHTTP(ctx, req)
	resp, err := d.client.Do(req)
	if err != nil {
		reqLog.Warn("transcoder cache lookup failed", "error", err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil // 404 = not conditioned yet
	}
	var out transcode.Conditioned
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil
	}
	return &out
}

// warmCondition triggers a real (blocking, cached) conditioning of the winner in
// the background, so the NEXT viewer of this ad gets seamless segments. Fire-
// and-forget with a detached context so it survives this request.
func (d *stitcherDeps) warmCondition(winner *sspWinner, p transcode.Profile) {
	if d.transcoderURL == "" || winner.CreativeID == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Minute)
		defer cancel()
		body, _ := json.Marshal(map[string]any{
			"creative_id": winner.CreativeID,
			"media_url":   winner.MediaURL,
			"profile":     p,
		})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.transcoderURL+routes.TranscodeCondition, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		// Dedicated long-timeout client: a cold conditioning is minutes of
		// ffmpeg, far past the 4s serving client, and we must not cancel it.
		if resp, err := (&http.Client{Timeout: 6 * time.Minute}).Do(req); err == nil {
			resp.Body.Close()
		}
	}()
}

// originManifest returns the content manifest to stitch: the request's ?origin=
// URL if given, else the configured ssai.origin_url, else the built-in sample.
// Fetching a real origin makes the stitcher a true proxy-and-stitch rather than
// a hardcoded playlist. Falls back to the sample on any fetch error so the demo
// never breaks.
// Returns the manifest text and the origin URL it came from ("" for the built-in
// sample), so the caller can resolve relative content-segment URIs to absolute.
func (d *stitcherDeps) originManifest(ctx context.Context, r *http.Request, channel string, reqLog *slog.Logger) (string, string) {
	sample := sampleContent
	if channel == constants.ChannelAudio {
		sample = sampleAudioContent
	}
	origin := r.URL.Query().Get("origin")
	if origin == "" && d.originFn != nil {
		origin = d.originFn()
	}
	if origin == "" {
		return sample, ""
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin, nil)
	if err != nil {
		reqLog.Warn("ssai origin request build failed, using sample", "origin", origin, "error", err)
		return sample, ""
	}
	resp, err := d.client.Do(req)
	if err != nil {
		reqLog.Warn("ssai origin fetch failed, using sample", "origin", origin, "error", err)
		return sample, ""
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		reqLog.Warn("ssai origin fetch non-200, using sample", "origin", origin, "status", resp.StatusCode)
		return sample, ""
	}
	reqLog.Info("ssai stitching real origin", "origin", origin, "channel", channel, "bytes", len(body))
	return string(body), origin
}

// resolveContentURIs rewrites relative content-segment URIs to absolute URLs
// against the origin manifest's location. The stitched manifest is re-served
// from the SSAI host, not the content host, so relative URIs (seg_0.ts) would
// otherwise resolve against the wrong base and 404 in the player.
func resolveContentURIs(m *ssai.Manifest, originURL string) {
	if originURL == "" {
		return
	}
	base, err := url.Parse(originURL)
	if err != nil {
		return
	}
	for i := range m.Segments {
		u, err := url.Parse(m.Segments[i].URI)
		if err != nil || u.IsAbs() {
			continue
		}
		m.Segments[i].URI = base.ResolveReference(u).String()
	}
}

// serveMaster rewrites each variant URI in an ABR master to a stitcher URL
// (?origin=<variant abs>&rung=<height> + the viewer's params), so hls.js fetches
// each rung through this stitcher and gets an ad conditioned to that rung.
func (d *stitcherDeps) serveMaster(w http.ResponseWriter, r *http.Request, master, masterURL string, reqLog *slog.Logger) {
	variants := ssai.ParseMaster(master)
	baseU, _ := url.Parse(masterURL)
	for i := range variants {
		abs := variants[i].URI
		if u, err := url.Parse(abs); err == nil && !u.IsAbs() && baseU != nil {
			abs = baseU.ResolveReference(u).String()
		}
		variants[i].URI = d.variantStitchURL(r, abs, variants[i].Height)
	}
	reqLog.Info("ssai master rewritten", "rungs", len(variants))
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, ssai.BuildMaster(variants))
}

// variantStitchURL builds a browser-reachable stitcher URL that stitches one
// variant playlist, carrying the viewer's params and the rung to condition to.
func (d *stitcherDeps) variantStitchURL(r *http.Request, variantAbsURL string, height int) string {
	q := url.Values{}
	for k, v := range r.URL.Query() {
		if k == "origin" || k == "rung" || k == "t" {
			continue
		}
		q[k] = append([]string(nil), v...)
	}
	q.Set("origin", variantAbsURL)
	q.Set("rung", strconv.Itoa(height))
	return strings.TrimRight(d.publicURL, "/") + routes.SSAIManifest + "?" + q.Encode()
}

// profileForRung picks the ladder profile matching the request's ?rung= (so the
// ad is conditioned to the same rung the player is watching); default otherwise.
func (d *stitcherDeps) profileForRung(r *http.Request) transcode.Profile {
	if rung := r.URL.Query().Get("rung"); rung != "" {
		for _, p := range transcode.DefaultLadder() {
			if strconv.Itoa(p.Height) == rung || p.RungName() == rung {
				return p
			}
		}
	}
	return transcode.DefaultProfile()
}

// serveContentManifest returns the sample origin content manifest (with ad-break
// markers) so the stitcher has something to rewrite in the demo. ?channel=audio
// serves the audio sample.
func serveContentManifest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	sample := sampleContent
	if channelFor(r) == constants.ChannelAudio {
		sample = sampleAudioContent
	}
	_, _ = io.WriteString(w, sample)
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
	BidModel         string  `json:"bid_model"`
	ClearingPrice    float64 `json:"clearing_price"`
	Currency         string  `json:"currency"`
	Width            int     `json:"width"`
	Height           int     `json:"height"`
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

	channel := channelFor(r)
	manifest, originURL := d.originManifest(ctx, r, channel, reqLog)

	// ABR: if the origin is a master playlist, rewrite each variant URI to point
	// back at this stitcher (so every rung is stitched independently, with the
	// ad conditioned to that rung's profile) and re-serve the master. Audio is
	// single-rendition — no ladder — so this only applies to video.
	if channel == constants.ChannelVideo && ssai.IsMaster(manifest) {
		d.serveMaster(w, r, manifest, originURL, reqLog)
		return
	}

	m, err := ssai.ParseMedia(manifest)
	if err != nil {
		http.Error(w, "content manifest parse failed", http.StatusInternalServerError)
		return
	}
	// Make content-segment URIs absolute so the player fetches them from the
	// content host, not the SSAI host that re-serves the stitched manifest.
	resolveContentURIs(m, originURL)

	// Session id lets the segment beacons correlate back to this stitch. Derive
	// from the trace so all beacons share the auction trace where possible.
	session := traceID
	if session == "" {
		session = fmt.Sprintf("ssai-%d", time.Now().UnixMilli())
	}

	// Condition ads to the audio-only profile for audio, else the video rung the
	// player is watching.
	adProfile := d.profileForRung(r)
	if channel == constants.ChannelAudio {
		adProfile = transcode.DefaultAudioProfile()
	}
	filled := 0
	m.Stitch(func(i int, span ssai.BreakSpan) []ssai.Segment {
		segs := d.fillBreak(ctx, r, channel, i, span, session, adProfile, reqLog)
		if len(segs) > 0 {
			filled++
		}
		return segs
	})

	reqLog.Info("ssai manifest stitched",
		"channel", channel, "breaks", len(m.Breaks()), "filled", filled, "ad_seconds", m.AdDuration())

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, m.Render())
}

// fillBreak builds the ad segments for one avail. It runs back-to-back auctions
// (an ad pod), conditioning each winner to the content profile, until the avail
// duration is filled or maxPodAds is reached. Each ad fires its own impression
// (on first-segment fetch) and full quartile set over its own duration, with a
// discontinuity at its boundary. If nothing fills, it splices a slate (when
// configured) or keeps content; a winner that can't be conditioned fires a VAST
// error beacon and is warmed for next time.
func (d *stitcherDeps) fillBreak(ctx context.Context, r *http.Request, channel string, breakIdx int, span ssai.BreakSpan, session string, adProfile transcode.Profile, reqLog *slog.Logger) []ssai.Segment {
	maxAds := d.maxPodAds()
	var out []ssai.Segment
	var filledDur float64
	ads := 0
	sawError := false
	seen := map[string]bool{}

	for ads < maxAds {
		remaining := span.Duration - filledDur
		if remaining < 1.0 {
			break // avail full
		}
		winner := d.runAuction(ctx, r, channel, remaining, reqLog)
		if winner == nil || winner.NoBid || winner.MediaURL == "" {
			break // no more demand for this avail
		}
		if winner.CreativeID != "" && seen[winner.CreativeID] {
			break // same ad again → stop (no duplicate creatives inside one pod)
		}
		seen[winner.CreativeID] = true

		adTrace := winner.TraceID
		if adTrace == "" {
			adTrace = fmt.Sprintf("%s-b%d-p%d", session, breakIdx, ads)
		}
		// Build the same MacroContext the publisher-adserver uses, so every beacon
		// SSAI fires is the identical HMAC-signed URL the tracker expects (passes
		// signature_validation, unlike a hand-rolled sig).
		mc := macroCtxFor(winner, adTrace, d.trackerURL)

		// Cache-only conditioning: never block serving on ffmpeg.
		cond := d.conditionCached(ctx, winner, adProfile, reqLog)
		if cond == nil || len(cond.Segments) == 0 {
			d.metrics.recordCond(false)
			d.warmCondition(winner, adProfile) // ready for the next viewer
			// VAST error: the winning ad couldn't be served this time.
			d.fireBeacon(ctx, eventBeacon(mc, channel, "error"))
			reqLog.Info("ssai pod ad not conditioned; error beacon + warming",
				"break", breakIdx, "pod", ads, "creative", winner.CreativeID)
			sawError = true
			break // stop the pod; fall through to slate/content
		}
		d.metrics.recordCond(true)
		out = append(out, d.adSegments(cond, mc, channel, session, adTrace, breakIdx, false)...)
		filledDur += cond.Duration
		ads++
	}

	if len(out) > 0 {
		d.metrics.recordBreak("filled")
		d.metrics.recordPod(ads, filledDur)
		reqLog.Info("ssai avail filled", "break", breakIdx, "pod_ads", ads, "ad_seconds", filledDur)
		return out
	}

	// Nothing filled: prefer a slate over dropping to content.
	if slate := d.slateSegments(ctx, adProfile, session, breakIdx, reqLog); len(slate) > 0 {
		d.metrics.recordBreak("slate")
		reqLog.Info("ssai avail slated", "break", breakIdx)
		return slate
	}
	if sawError {
		d.metrics.recordBreak("error")
	} else {
		d.metrics.recordBreak("unfilled")
	}
	reqLog.Info("ssai break unfilled, keeping content", "break", breakIdx)
	return nil
}

// adSegments turns a conditioned ad into stitched manifest segments. For a real
// ad it attaches the impression to the first segment and the VAST quartiles each
// segment crosses; a slate carries no beacons. The first segment gets a
// discontinuity so the decoder resets at the content↔ad (and ad↔ad) boundary.
func (d *stitcherDeps) adSegments(cond *transcode.Conditioned, mc adserving.MacroContext, channel, session, adTrace string, breakIdx int, isSlate bool) []ssai.Segment {
	var impression string
	if !isSlate {
		impression = adserving.BuildImpressionURL(mc)
	}
	segs := make([]ssai.Segment, len(cond.Segments))
	var start float64
	for n, cs := range cond.Segments {
		var evs, beacons []string
		if !isSlate {
			// Fire every VAST quartile whose time-mark the segment crosses (a
			// segment can cross more than one; short ads still emit all five).
			evs = eventsForSegment(start, cs.Duration, cond.Duration)
			beacons = make([]string, 0, len(evs)+1)
			if n == 0 {
				beacons = append(beacons, impression)
			}
			for _, ev := range evs {
				beacons = append(beacons, eventBeacon(mc, channel, ev))
			}
		}
		segs[n] = ssai.Segment{Duration: cs.Duration, URI: d.segmentURL(session, adTrace, breakIdx, n, evs, beacons, cs.URI)}
		start += cs.Duration
	}
	if len(segs) > 0 {
		segs[0].Discontinuity = true
		// fMP4/CMAF: the ad carries its own init; declare it on the first ad
		// segment (the player then decodes ad segments against the ad init, and
		// Stitch restores the content init on the segment after the break).
		segs[0].Map = cond.InitURI
	}
	return segs
}

// eventBeacon builds the signed quartile/error tracker URL for the channel:
// /v1/t/audio for audio, /v1/t/video otherwise. The impression (/v1/t/imp) is
// channel-agnostic and built directly.
func eventBeacon(mc adserving.MacroContext, channel, event string) string {
	if channel == constants.ChannelAudio {
		return adserving.BuildAudioEventURL(mc, event)
	}
	return adserving.BuildVideoEventURL(mc, event)
}

// channelFor reads the request's ?channel= (audio|video), defaulting to video.
func channelFor(r *http.Request) string {
	if strings.ToLower(r.URL.Query().Get("channel")) == constants.ChannelAudio {
		return constants.ChannelAudio
	}
	return constants.ChannelVideo
}

// slateSegments conditions and returns the configured slate clip for an unfilled
// avail (cache-only, no beacons). Returns nil when no slate is configured or the
// slate isn't conditioned yet (in which case it warm-conditions it for next time
// and the caller keeps content).
func (d *stitcherDeps) slateSegments(ctx context.Context, adProfile transcode.Profile, session string, breakIdx int, reqLog *slog.Logger) []ssai.Segment {
	if d.slateCreativeFn == nil {
		return nil
	}
	slateID := d.slateCreativeFn()
	if slateID == "" {
		return nil
	}
	mediaURL := ""
	if d.slateMediaFn != nil {
		mediaURL = d.slateMediaFn()
	}
	winner := &sspWinner{CreativeID: slateID, MediaURL: mediaURL}
	cond := d.conditionCached(ctx, winner, adProfile, reqLog)
	if cond == nil || len(cond.Segments) == 0 {
		if mediaURL != "" {
			d.warmCondition(winner, adProfile) // ready next time
		}
		return nil
	}
	mc := macroCtxFor(winner, session, d.trackerURL)
	return d.adSegments(cond, mc, constants.ChannelVideo, session, "slate-"+session, breakIdx, true)
}

// runAuction calls the SSP for the given channel (audio|video), forwarding the
// viewer's signals so the ad is targeted to the real request (not a hardcoded
// default).
func (d *stitcherDeps) runAuction(ctx context.Context, r *http.Request, channel string, breakDur float64, reqLog *slog.Logger) *sspWinner {
	q := url.Values{}
	for k, v := range r.URL.Query() {
		q[k] = append([]string(nil), v...)
	}
	q.Set("channel", channel)
	if q.Get("placement_id") == "" {
		q.Set("placement_id", d.placementFn())
	}
	if q.Get("geo") == "" {
		q.Set("geo", "USA")
	}
	if q.Get("device") == "" {
		// SSAI is overwhelmingly CTV/OTT for video; audio DAI is mobile/smart-speaker.
		dev := "ctv"
		if channel == constants.ChannelAudio {
			dev = "mobile"
		}
		q.Set("device", dev)
	}
	// Advertise the remaining avail so demand can return an ad that fits the pod
	// slot (a real SSP honours max ad duration; the built-in demo ignores it).
	if breakDur > 0 {
		q.Set("max_duration", strconv.Itoa(int(breakDur)))
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
	// beacon values are the pre-signed, HMAC-valid tracker URLs built at stitch
	// time (a segment may carry several: the impression plus any quartiles it
	// crosses). Firing them here (not reconstructing them) is what keeps SSAI's
	// beacons identical to the ones the player would fire in the client-side flow.
	for _, beacon := range q["beacon"] {
		if beacon != "" {
			d.fireBeacon(r.Context(), beacon)
		}
	}
	if redir == "" {
		http.Error(w, "missing redir", http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, redir, http.StatusFound)
}

// segmentURL builds the manifest URI for one ad segment: a call back into this
// service's segment beacon endpoint (via the browser-reachable public URL),
// carrying the pre-signed beacon(s) to fire and the real media to redirect to.
// events is kept as a plain param for readability/debugging of the manifest.
func (d *stitcherDeps) segmentURL(session, adTrace string, adIdx, n int, events, beacons []string, mediaURL string) string {
	q := url.Values{}
	q.Set("session", session)
	q.Set("ad", adTrace)
	q.Set("break", strconv.Itoa(adIdx))
	q.Set("seg", strconv.Itoa(n))
	if len(events) > 0 {
		q.Set("event", strings.Join(events, ","))
	}
	q["beacon"] = beacons
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

// eventsForSegment returns the VAST tracking events whose time-marks fall within
// the ad segment [start, start+segDur) of a total-second ad: start (t=0),
// firstQuartile (25%), midpoint (50%), thirdQuartile (75%), complete (100%). A
// single segment can cross several marks — a 15s ad in two 6s segments plus a 3s
// tail still emits all five — which is why this returns a slice rather than one
// event per segment. Marks are assigned to the half-open interval that contains
// them (a mark exactly on a boundary belongs to the following segment); complete
// is assigned to the last segment (end >= total).
func eventsForSegment(start, segDur, total float64) []string {
	if total <= 0 {
		return nil
	}
	const eps = 0.001
	end := start + segDur
	marks := []struct {
		name string
		t    float64
	}{
		{"start", 0},
		{"firstQuartile", 0.25 * total},
		{"midpoint", 0.5 * total},
		{"thirdQuartile", 0.75 * total},
	}
	var evs []string
	for _, m := range marks {
		if m.t >= start-eps && m.t < end-eps {
			evs = append(evs, m.name)
		}
	}
	if end >= total-eps {
		evs = append(evs, "complete")
	}
	return evs
}

// macroCtxFor builds the beacon-signing context for a winner — the same shape
// the publisher-adserver uses, so BuildImpressionURL / BuildVideoEventURL emit
// the identical HMAC-signed tracker URLs the platform expects.
func macroCtxFor(wn *sspWinner, adTrace, trackerURL string) adserving.MacroContext {
	cur := wn.Currency
	if strings.TrimSpace(cur) == "" {
		cur = "USD"
	}
	bm := wn.BidModel
	if strings.TrimSpace(bm) == "" {
		bm = "cpm"
	}
	return adserving.MacroContext{
		AuctionID:    adTrace,
		AuctionPrice: wn.ClearingPrice,
		Currency:     cur,
		CampaignID:   wn.CampaignID,
		CreativeID:   wn.CreativeID,
		PlacementID:  wn.PlacementID,
		PublisherID:  wn.PublisherID,
		AdvertiserID: wn.AdvertiserID,
		BidModel:     bm,
		DealID:       "",
		Width:        wn.Width,
		Height:       wn.Height,
		TrackerURL:   trackerURL,
		URLTTL:       time.Hour,
	}
}
