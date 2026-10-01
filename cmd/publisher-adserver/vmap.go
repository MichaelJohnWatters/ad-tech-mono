package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/vmap"
)

// vmapHandler returns a VMAP 1.0 schedule with three breaks (pre-roll
// at content start, mid-roll at 00:00:30, post-roll at content end).
// Each AdBreak's AdSource is an AdTagURI pointing back at the VAST
// endpoint with a break_id query param. The IMA SDK fetches a fresh
// VAST per break at the right moment in playback, so each break runs
// its own independent auction. This matches how production long-form
// publishers wire VMAP — one schedule, N independent VAST auctions.
//
// publicBase is the browser-reachable URL prefix the player will use
// to call back into our gateway. For the demo it defaults to
// http://localhost:8080 (overridable via publisher_adserver.public_url
// when this service runs behind a different host).
func vmapHandler(log *slog.Logger, publicBase string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		traceID := tracing.TraceIDFromContext(ctx)
		if traceID == "" {
			traceID = fmt.Sprintf("vmap-%d", time.Now().UnixMilli())
		}
		reqLog := logger.WithContext(log, logger.WithTraceID(ctx, traceID))

		placement := r.URL.Query().Get("placement_id")
		if placement == "" {
			placement = "pl-sport-mpu"
		}

		// Carry the visitor's geo/device/consent/identity signals onto every
		// break's VAST tag so each per-break auction runs on the real request.
		// We forward only params the caller actually sent — no USA/desktop
		// defaults — because the VAST handler defaults at fetch time; baking
		// defaults into the schedule would mislabel a real EU/mobile viewer.
		// break=<pre|mid|post> is appended per-break below.
		vastParams := url.Values{}
		for k, v := range r.URL.Query() {
			vastParams[k] = append([]string(nil), v...)
		}
		vastParams.Set("placement_id", placement)
		vastBase := publicBase + routes.PublisherAdServeVAST + "?" + vastParams.Encode()

		// Break-start/-end beacons hit the signature-gated /v1/t/view handler, so
		// they must carry a valid HMAC `sig` or they 403 under strict signing
		// (tracker.signature_validation=true) — silently dropping CTV break
		// tracking. Sign each through the same helper the impression/audio
		// trackers use; the signature covers every param (ev/br included), so the
		// tracker validates it exactly like an adserver-built viewability URL.
		breakTracker := func(ev, br string) string {
			return adserving.SignURL(
				publicBase+"/v1/t/view?tid="+traceID+"&ev="+ev+"&br="+br,
				adserving.ActiveSigningKey())
		}

		// The three break kinds, keyed by name. Mid-roll uses a PERCENT offset (50%)
		// rather than an absolute 00:00:30 so it still fires on short demo clips (a
		// 30s absolute offset never triggers on a 10s content clip).
		breakByName := map[string]vmap.BreakSpec{
			"pre": {
				BreakID:       "pre-roll",
				Offset:        vmap.TimeOffset{Start: true},
				AdTagURL:      vastBase + "&break=pre",
				AdTagTemplate: "vast4.2",
				Trackers:      vmap.BreakTrackers{BreakStart: []string{breakTracker("break_start", "pre")}, BreakEnd: []string{breakTracker("break_end", "pre")}},
			},
			"mid": {
				BreakID:       "mid-roll-1",
				Offset:        vmap.TimeOffset{PercentOf: 50},
				AdTagURL:      vastBase + "&break=mid",
				AdTagTemplate: "vast4.2",
				Trackers:      vmap.BreakTrackers{BreakStart: []string{breakTracker("break_start", "mid")}, BreakEnd: []string{breakTracker("break_end", "mid")}},
			},
			"post": {
				BreakID:       "post-roll",
				Offset:        vmap.TimeOffset{End: true},
				AdTagURL:      vastBase + "&break=post",
				AdTagTemplate: "vast4.2",
				Trackers:      vmap.BreakTrackers{BreakStart: []string{breakTracker("break_start", "post")}, BreakEnd: []string{breakTracker("break_end", "post")}},
			},
		}
		// ?breaks= selects which breaks to schedule (CSV subset of pre,mid,post), in
		// that canonical order. Default = all three (back-compat). Unknown names are
		// ignored; an empty/invalid result falls back to all three.
		specs := selectBreaks(r.URL.Query().Get("breaks"), breakByName)

		xmlBytes, err := vmap.BuildSchedule(specs)
		if err != nil {
			reqLog.Error("vmap build failed", "error", err)
			http.Error(w, "vmap build failed", http.StatusInternalServerError)
			return
		}
		reqLog.Info("vmap schedule served", "trace_id", traceID, "breaks", len(specs), "placement", placement)
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(xmlBytes)
	}
}

// selectBreaks resolves the ?breaks= CSV (subset of pre,mid,post) into the ordered
// BreakSpecs to schedule. Canonical order (pre→mid→post) is always preserved
// regardless of the CSV order. Empty or all-unknown input returns all three breaks
// (back-compat: the old handler always scheduled pre+mid+post).
func selectBreaks(csv string, byName map[string]vmap.BreakSpec) []vmap.BreakSpec {
	order := []string{"pre", "mid", "post"}
	want := map[string]bool{}
	for _, name := range strings.Split(csv, ",") {
		name = strings.TrimSpace(strings.ToLower(name))
		if _, ok := byName[name]; ok {
			want[name] = true
		}
	}
	if len(want) == 0 { // empty or unknown → all three
		for _, name := range order {
			want[name] = true
		}
	}
	out := make([]vmap.BreakSpec, 0, len(order))
	for _, name := range order {
		if want[name] {
			out = append(out, byName[name])
		}
	}
	return out
}
