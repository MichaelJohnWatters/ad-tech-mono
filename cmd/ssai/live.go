package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ssai"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/transcode"
)

// Live channel — a CONTINUOUS sliding-window HLS stream, as opposed to the finite
// VOD stitch of manifestHandler. The player sits at the live edge and re-polls;
// content loops over a wall-clock timeline, and a fresh ad break (its own auction)
// is injected every ~break_every seconds. This is what makes Twitchr feel like a
// real live channel instead of a clip that plays once.
//
// Timeline model (deterministic so every replica + every poll agree):
//   - segDur-second segments, indexed by t = floor((now - liveEpoch)/segDur).
//   - cycle = contentRun content segments then 1 ad segment; contentRun ≈
//     break_every/segDur, so a break lands every ~break_every seconds of content.
//   - the playlist is the window [pos-liveWindow, pos): MEDIA-SEQUENCE = window
//     start, no #EXT-X-ENDLIST → hls.js plays it as live.
//
// liveEpoch is a FIXED anchor (not process start) so the 3 stitcher replicas
// compute the identical timeline — a player's polls are load-balanced across pods,
// and a per-pod epoch would make the stream jump between pods.
var liveEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

const (
	liveWindow      = 6  // segments in the live window (~live edge depth)
	liveAdKeepBack  = 24 // evict ad-slot cache entries older than this many breaks
	liveDefaultSegS = 6.0
)

// liveContentCache memoises the (static) content segment list per origin.
var (
	liveContentMu    sync.Mutex
	liveContentCache = map[string][]ssai.Segment{}
)

// liveAdCache holds the decided ad segment(s) for a given break ordinal, shared
// across viewers (broadcast model: one ad airs per break). Keyed "placement|ord".
var (
	liveAdMu    sync.Mutex
	liveAdCache = map[string][]ssai.Segment{}
)

// liveManifestHandler serves the continuous live channel playlist.
func (d *stitcherDeps) liveManifestHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	traceID := tracing.TraceIDFromContext(ctx)
	reqLog := logger.WithContext(log, logger.WithTraceID(ctx, traceID))
	channel := channelFor(r)

	content := d.liveContentSegments(ctx, r, channel, reqLog)
	if len(content) == 0 {
		http.Error(w, "live content unavailable", http.StatusInternalServerError)
		return
	}
	segDur := content[0].Duration
	if segDur <= 0 {
		segDur = liveDefaultSegS
	}
	breakEvery := 15.0
	if v, err := strconv.ParseFloat(r.URL.Query().Get("break_every"), 64); err == nil && v >= segDur {
		breakEvery = v
	}
	contentRun := int(math.Round(breakEvery / segDur))
	if contentRun < 1 {
		contentRun = 1
	}
	cycle := contentRun + 1 // + one ad segment per break

	pos := int(math.Floor(time.Since(liveEpoch).Seconds() / segDur))
	startIdx := pos - liveWindow
	if startIdx < 0 {
		startIdx = 0
	}

	adProfile := d.profileForRung(r)
	placement := r.URL.Query().Get("placement_id")
	if placement == "" && d.placementFn != nil {
		placement = d.placementFn()
	}

	isAd := func(t int) bool { return t >= 0 && t%cycle == contentRun }

	var segs []ssai.Segment
	for t := startIdx; t < pos; t++ {
		disc := t >= 1 && isAd(t) != isAd(t-1) // discontinuity at each content↔ad boundary
		if isAd(t) {
			breakOrd := t / cycle
			ad := d.liveAdForBreak(ctx, r, channel, placement, breakOrd, adProfile, reqLog)
			if len(ad) > 0 {
				ad[0].Discontinuity = true
				segs = append(segs, ad[0]) // one ad segment per live break slot
				continue
			}
			// No ad/slate available → show content in the slot (never gap the stream).
		}
		cs := content[((t%len(content))+len(content))%len(content)]
		segs = append(segs, ssai.Segment{Duration: cs.Duration, URI: cs.URI, Map: cs.Map, Discontinuity: disc})
	}

	m := &ssai.Manifest{
		Header: []string{
			"#EXTM3U",
			"#EXT-X-VERSION:3",
			fmt.Sprintf("#EXT-X-TARGETDURATION:%d", int(math.Ceil(segDur))),
			fmt.Sprintf("#EXT-X-MEDIA-SEQUENCE:%d", startIdx),
			fmt.Sprintf("#EXT-X-DISCONTINUITY-SEQUENCE:%d", liveDiscSeq(startIdx, cycle, contentRun)),
		},
		Segments: segs,
		EndList:  false, // live: never terminate
	}
	reqLog.Info("ssai live window served", "channel", channel, "media_seq", startIdx,
		"segments", len(segs), "cycle", cycle, "content_run", contentRun, "break_every", breakEvery)
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, m.Render())
}

// liveDiscSeq is the #EXT-X-DISCONTINUITY-SEQUENCE: the number of content↔ad
// boundaries strictly before the window's first segment (startIdx). Computed in
// O(1) from the cycle pattern (the timeline is far too long to iterate). With one
// ad segment per cycle there are two boundaries per cycle: content→ad at offset
// contentRun, and ad→content at offset 0 (t≥cycle).
func liveDiscSeq(startIdx, cycle, contentRun int) int {
	n := startIdx - 1 // boundaries on segments with index in [1, startIdx-1]
	if n < 1 {
		return 0
	}
	// content→ad boundaries: t in [1,n], t%cycle==contentRun
	toAd := 0
	if n >= contentRun && contentRun >= 1 {
		toAd = (n-contentRun)/cycle + 1
	}
	// ad→content boundaries: t in [1,n], t%cycle==0 (t≥cycle)
	toContent := n / cycle
	return toAd + toContent
}

// liveContentSegments returns the content segment list for the live origin
// (resolved to a media playlist, URIs absolute, break markers stripped), memoised
// per origin since content files are static.
func (d *stitcherDeps) liveContentSegments(ctx context.Context, r *http.Request, channel string, reqLog *slog.Logger) []ssai.Segment {
	cacheKey := r.URL.Query().Get("origin") + "|" + channel
	liveContentMu.Lock()
	if segs, ok := liveContentCache[cacheKey]; ok {
		liveContentMu.Unlock()
		return segs
	}
	liveContentMu.Unlock()

	manifest, originURL := d.originManifest(ctx, r, channel, reqLog)
	if ssai.IsMaster(manifest) {
		if vs := ssai.ParseMaster(manifest); len(vs) > 0 {
			v := vs[0] // lowest rung is plenty for the live demo
			abs := v.URI
			if originURL != "" {
				if base, e := url.Parse(originURL); e == nil {
					if u, e2 := url.Parse(v.URI); e2 == nil {
						abs = base.ResolveReference(u).String()
					}
				}
			}
			if txt, ok := d.readManifest(ctx, abs, reqLog); ok {
				manifest, originURL = txt, abs
			}
		}
	}
	m, err := ssai.ParseMedia(manifest)
	if err != nil {
		reqLog.Warn("live content parse failed", "error", err)
		return nil
	}
	resolveContentURIs(m, originURL)
	out := make([]ssai.Segment, 0, len(m.Segments))
	for _, s := range m.Segments {
		// Strip the VOD break markers — the live timeline inserts its own breaks.
		out = append(out, ssai.Segment{Duration: s.Duration, URI: s.URI, Map: s.Map})
	}
	liveContentMu.Lock()
	liveContentCache[cacheKey] = out
	liveContentMu.Unlock()
	return out
}

// liveAdForBreak returns the ad segment(s) for a live break ordinal, running a
// fresh auction the first time that break is seen and caching the result (shared
// across viewers). Falls back to a slate, else nil (the caller shows content).
func (d *stitcherDeps) liveAdForBreak(ctx context.Context, r *http.Request, channel, placement string, breakOrd int, adProfile transcode.Profile, reqLog *slog.Logger) []ssai.Segment {
	key := placement + "|" + strconv.Itoa(breakOrd)
	liveAdMu.Lock()
	if segs, ok := liveAdCache[key]; ok {
		liveAdMu.Unlock()
		return segs
	}
	liveAdMu.Unlock()

	// One fresh trace per break (its own win/impression/quartile unit).
	adTrace, traceparent := tracing.NewClientTraceparent()
	session := adTrace
	var result []ssai.Segment

	winner := d.runAuction(ctx, r, channel, 0, traceparent, reqLog)
	if winner != nil && !winner.NoBid && winner.MediaURL != "" {
		if cond := d.conditionCached(ctx, winner, adProfile, reqLog); cond != nil && len(cond.Segments) > 0 {
			mc := macroCtxFor(winner, adTrace, d.trackerURL)
			built := d.adSegments(cond, mc, channel, session, adTrace, breakOrd, false)
			if len(built) > 0 {
				result = built[:1] // one segment per live slot (impression rides seg 0)
				d.recordFreqCap(ctx, r, channel, winner, adTrace)
			}
		} else {
			d.warmCondition(winner, adProfile) // ready for a later break
		}
	}
	if len(result) == 0 {
		if slate := d.slateSegments(ctx, adProfile, session, breakOrd, reqLog); len(slate) > 0 {
			result = slate[:1]
		}
	}

	liveAdMu.Lock()
	liveAdCache[key] = result
	// Evict stale breaks so the cache stays bounded.
	for k := range liveAdCache {
		if i := indexAfterPipe(k); i >= 0 {
			if ord, err := strconv.Atoi(k[i:]); err == nil && ord < breakOrd-liveAdKeepBack {
				delete(liveAdCache, k)
			}
		}
	}
	liveAdMu.Unlock()
	return result
}

func indexAfterPipe(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '|' {
			return i + 1
		}
	}
	return -1
}
