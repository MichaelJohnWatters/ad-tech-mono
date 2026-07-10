package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/dash"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ssai"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/transcode"
)

// serveMultiRungDASH emits an ABR DASH MPD with one Representation per rung. It
// decides the ads ONCE (per break) and keeps only ads conditioned on EVERY rung,
// so the period structure is identical across rungs — the precondition
// dash.AssembleMultiRung needs. Ads missing on any rung are warm-conditioned and
// dropped this request (content plays until every rung is warm — the prewarm job
// keeps them warm in steady state). Returns false to let the caller fall back to
// single-rung DASH (too few rungs / unreadable variants).
func (d *stitcherDeps) serveMultiRungDASH(ctx context.Context, w http.ResponseWriter, r *http.Request, master, masterURL string, reqLog *slog.Logger) bool {
	variants := ssai.ParseMaster(master)
	if len(variants) < 2 {
		return false
	}
	session := tracing.TraceIDFromContext(ctx)
	if session == "" {
		session, _ = tracing.NewClientTraceparent()
	}
	ladder := transcode.DefaultLadder()
	if d.ladderFn != nil {
		ladder = d.ladderFn()
	}
	baseU, _ := url.Parse(masterURL)
	abs := func(ref string) string {
		if u, err := url.Parse(ref); err == nil && !u.IsAbs() && baseU != nil {
			return baseU.ResolveReference(u).String()
		}
		return ref
	}

	type rungCtx struct {
		rep     dash.RepInfo
		profile transcode.Profile
		content *ssai.Manifest
	}
	var rungs []rungCtx
	for _, v := range variants {
		prof := profileByHeight(ladder, v.Height)
		prof.Container = transcode.ContainerCMAF
		cu := abs(v.URI)
		txt, ok := d.readManifest(ctx, cu, reqLog)
		if !ok {
			continue
		}
		cm, err := ssai.ParseMedia(txt)
		if err != nil {
			continue
		}
		resolveContentURIs(cm, cu)
		rungs = append(rungs, rungCtx{rep: repInfoForProfile(prof), profile: prof, content: cm})
	}
	if len(rungs) < 2 {
		return false
	}

	// Ad decisions per break (from the base rung's break structure), kept only
	// when conditioned on every rung.
	breaks := rungs[0].content.Breaks()
	breakAds := make([][]*sspWinner, len(breaks))
	filled := 0
	for bi, span := range breaks {
		var kept []*sspWinner
		for _, wn := range d.podWinners(ctx, r, constants.ChannelVideo, span, reqLog) {
			cachedOnAll := true
			for _, rc := range rungs {
				if c := d.conditionCached(ctx, wn, rc.profile, reqLog); c == nil || len(c.Segments) == 0 {
					cachedOnAll = false
					d.warmCondition(wn, rc.profile)
				}
			}
			if cachedOnAll {
				kept = append(kept, wn)
			}
		}
		breakAds[bi] = kept
		if len(kept) > 0 {
			filled++
		}
	}

	// Stitch each rung with the shared ad decisions.
	rungInputs := make([]dash.RungInput, 0, len(rungs))
	for _, rc := range rungs {
		rc.content.Stitch(func(i int, _ ssai.BreakSpan) []ssai.Segment {
			var segs []ssai.Segment
			for _, wn := range breakAds[i] {
				cond := d.conditionCached(ctx, wn, rc.profile, reqLog)
				if cond == nil || len(cond.Segments) == 0 {
					continue
				}
				mc := macroCtxFor(wn, wn.podTrace, d.trackerURL)
				segs = append(segs, d.adSegments(cond, mc, constants.ChannelVideo, session, wn.podTrace, i, false)...)
			}
			return segs
		})
		rungInputs = append(rungInputs, dash.RungInput{Rep: rc.rep, Segs: toDashSegs(rc.content)})
	}

	// AssembleMultiRung takes the period structure from rung 0 and assumes every
	// other rung matches it segment-for-segment. serveMultiRungDASH only guarantees
	// the same AD DECISIONS — each rung reads its OWN origin variant, so a
	// demuxed/differently-segmented content variant would misalign the periods
	// (AssembleMultiRung's `i < len(segs)` guard would silently drop/misplace a
	// rung's segments rather than error). Bail to the proven single-rung path when
	// the stitched rungs don't line up.
	if !rungsAligned(rungInputs) {
		reqLog.Warn("multi-rung DASH rungs not structurally aligned; falling back to single-rung",
			"rungs", len(rungInputs))
		return false
	}

	quartileEvents := d.timedMetadataFn != nil && d.timedMetadataFn()
	mpd := dash.AssembleMultiRung(rungInputs, quartileEvents)
	if d.omidFn != nil {
		if vendor, res := d.omidFn(); res != "" {
			mpd.AddOMID(vendor, res)
		}
	}
	xmlDoc, err := mpd.XML()
	if err != nil {
		reqLog.Error("multi-rung MPD marshal failed", "error", err)
		return false
	}
	reqLog.Info("ssai multi-rung DASH stitched", "rungs", len(rungs), "breaks", len(breaks), "filled", filled)
	w.Header().Set("Content-Type", "application/dash+xml")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, xmlDoc)
	return true
}

// podWinners runs the per-avail auction pod and returns the winners WITHOUT
// conditioning them (multi-rung conditions per rung afterwards). Mirrors
// fillBreak's pod loop; ad duration is estimated from the winner for the fill
// accounting since we don't condition here.
func (d *stitcherDeps) podWinners(ctx context.Context, r *http.Request, channel string, span ssai.BreakSpan, reqLog *slog.Logger) []*sspWinner {
	maxAds := d.maxPodAds()
	var out []*sspWinner
	var filled float64
	seen := map[string]bool{}
	for len(out) < maxAds {
		remaining := span.Duration - filled
		if remaining < 1.0 {
			break
		}
		// Distinct trace per pod ad (see fillBreak), carried on the winner so every
		// rung stitches the SAME ad with the SAME trace — one ad is one impression
		// regardless of which ABR rung the player fetches.
		podTrace, traceparent := tracing.NewClientTraceparent()
		wn := d.runAuction(ctx, r, channel, remaining, traceparent, reqLog)
		if wn == nil || wn.NoBid || wn.MediaURL == "" {
			break
		}
		if wn.CreativeID != "" && seen[wn.CreativeID] {
			break
		}
		seen[wn.CreativeID] = true
		wn.podTrace = podTrace
		out = append(out, wn)
		dur := float64(wn.DurationSeconds)
		if dur <= 0 {
			dur = remaining
		}
		filled += dur
	}
	return out
}

// rungsAligned reports whether every rung shares the SAME period structure —
// the precondition dash.AssembleMultiRung relies on. It requires equal segment
// count and, per index, the same content/ad flag, the same init-change position
// (a non-empty Init marks a period boundary), and matching durations. A false
// return means the rungs diverged (e.g. a differently-segmented origin variant)
// and the caller must fall back to single-rung rather than emit a misaligned MPD.
func rungsAligned(rungs []dash.RungInput) bool {
	if len(rungs) < 2 {
		return true
	}
	base := rungs[0].Segs
	for _, r := range rungs[1:] {
		if len(r.Segs) != len(base) {
			return false
		}
		for i := range base {
			if r.Segs[i].Ad != base[i].Ad {
				return false
			}
			if (r.Segs[i].Init != "") != (base[i].Init != "") {
				return false // init change (period boundary) at a different index
			}
			if d := r.Segs[i].Duration - base[i].Duration; d > 1e-3 || d < -1e-3 {
				return false
			}
		}
	}
	return true
}

// profileByHeight returns the ladder profile whose height matches h (the DASH
// content rung), or the default profile when none matches.
func profileByHeight(ladder []transcode.Profile, h int) transcode.Profile {
	for _, p := range ladder {
		if p.Height == h {
			return p
		}
	}
	return transcode.DefaultProfile()
}

// repInfoForProfile builds the DASH Representation info for a video profile.
func repInfoForProfile(p transcode.Profile) dash.RepInfo {
	return dash.RepInfo{
		ID: p.RungName(), Bandwidth: p.BandwidthBps(), Codecs: p.Codecs(),
		Width: p.Width, Height: p.Height, MimeType: "video/mp4",
	}
}

// toDashSegs maps a stitched manifest's segments to DASH assembly segments.
func toDashSegs(m *ssai.Manifest) []dash.Seg {
	out := make([]dash.Seg, 0, len(m.Segments))
	for _, s := range m.Segments {
		out = append(out, dash.Seg{Media: s.URI, Init: s.Map, Duration: s.Duration, Ad: s.Ad})
	}
	return out
}
