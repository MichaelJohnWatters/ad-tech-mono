package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

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

		specs := []vmap.BreakSpec{
			{
				BreakID:       "pre-roll",
				Offset:        vmap.TimeOffset{Start: true},
				AdTagURL:      vastBase + "&break=pre",
				AdTagTemplate: "vast4.2",
				Trackers: vmap.BreakTrackers{
					BreakStart: []string{publicBase + "/v1/t/view?tid=" + traceID + "&ev=break_start&br=pre"},
					BreakEnd:   []string{publicBase + "/v1/t/view?tid=" + traceID + "&ev=break_end&br=pre"},
				},
			},
			{
				BreakID:       "mid-roll-1",
				Offset:        vmap.TimeOffset{AbsoluteAt: 30 * time.Second},
				AdTagURL:      vastBase + "&break=mid",
				AdTagTemplate: "vast4.2",
				Trackers: vmap.BreakTrackers{
					BreakStart: []string{publicBase + "/v1/t/view?tid=" + traceID + "&ev=break_start&br=mid"},
					BreakEnd:   []string{publicBase + "/v1/t/view?tid=" + traceID + "&ev=break_end&br=mid"},
				},
			},
			{
				BreakID:       "post-roll",
				Offset:        vmap.TimeOffset{End: true},
				AdTagURL:      vastBase + "&break=post",
				AdTagTemplate: "vast4.2",
				Trackers: vmap.BreakTrackers{
					BreakStart: []string{publicBase + "/v1/t/view?tid=" + traceID + "&ev=break_start&br=post"},
					BreakEnd:   []string{publicBase + "/v1/t/view?tid=" + traceID + "&ev=break_end&br=post"},
				},
			},
		}

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
