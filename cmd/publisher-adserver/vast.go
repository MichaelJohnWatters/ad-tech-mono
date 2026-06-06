package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/vast"
)

// vastHandler serves a VAST 4.2 XML document players can consume to
// play a pre-roll / mid-roll ad. End-to-end demo wiring for Phase 9 —
// the response is a real VAST document but the auction it represents
// is a stub: a single LuxAuto pre-roll creative pointing at a public-
// CDN sample MP4. Subsequent steps (full DSP video bidding, transcoder
// output) replace the stubbed bits with the real flow without changing
// the wire format.
//
// Tracker URLs in the VAST are built via pkg/adserving.Build* — same
// HMAC-signed shape display creatives use, so quartile beacons and the
// click URL flow through the existing tracker dedup + signature gates.
// The player firing AdComplete results in a /v1/t/view request the
// tracker records exactly like an MPU viewability beacon.
func vastHandler(log *slog.Logger, trackerURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		traceID := tracing.TraceIDFromContext(ctx)
		if traceID == "" {
			traceID = fmt.Sprintf("vast-%d", time.Now().UnixMilli())
		}
		reqLog := logger.WithContext(log, logger.WithTraceID(ctx, traceID))

		// MacroContext drives the signed tracker URLs. CampaignID /
		// CreativeID / etc. are stub values for now (step 86 demo wiring);
		// the full DSP video flow will populate these from the winning bid
		// once it exists.
		macroCtx := adserving.MacroContext{
			AuctionID:    traceID,
			AuctionPrice: 14.50,
			Currency:     "USD",
			CampaignID:   "demo-video-li",
			CreativeID:   "demo-video-cr",
			PlacementID:  r.URL.Query().Get("placement_id"),
			PublisherID:  "demo-pub",
			AdvertiserID: "demo-luxauto",
			BidModel:     "cpm",
			Width:        640,
			Height:       360,
			TrackerURL:   trackerURL,
			LandingURL:   "http://localhost:8080/dev/landing/luxauto",
			URLTTL:       time.Hour,
		}

		impURL := adserving.BuildImpressionURL(macroCtx)
		clickURL := adserving.BuildClickURL(macroCtx)
		// Quartile / start / complete beacons all reuse the viewability
		// URL with an &ev= suffix so the tracker can branch on the
		// event in one handler. The sig covers the original params; the
		// appended ev is unsigned which is fine — events are publisher-
		// observable anyway and the dedup gate handles replay.
		viewURL := adserving.BuildViewabilityURL(macroCtx)
		beacon := func(ev string) string {
			sep := "?"
			if i := indexByte(viewURL, '?'); i >= 0 {
				sep = "&"
			}
			return viewURL + sep + "ev=" + ev
		}

		spec := vast.LinearSpec{
			AdID:       traceID,
			AdSystem:   "ad-tech-mono",
			AdTitle:    "LuxAuto Pre-Roll Demo",
			Advertiser: "luxauto.com",
			Duration:   15 * time.Second,
			// Sample MP4 from Google's public ad sample CDN. The transcoder
			// step (Phase 9 step 85) will replace this with our own hosted
			// renditions; for the demo this gives the player something
			// real to render without depending on local file uploads.
			MediaFiles: []vast.MediaFile{{
				Delivery: "progressive",
				Type:     "video/mp4",
				Bitrate:  800,
				Width:    640,
				Height:   360,
				URI:      "https://storage.googleapis.com/interactive-media-ads/media/android.mp4",
			}},
			Trackers: vast.LinearTrackers{
				Impression:    []string{impURL},
				Start:         []string{beacon("start")},
				FirstQuartile: []string{beacon("firstQuartile")},
				Midpoint:      []string{beacon("midpoint")},
				ThirdQuartile: []string{beacon("thirdQuartile")},
				Complete:      []string{beacon("complete")},
				Mute:          []string{beacon("mute")},
				Pause:         []string{beacon("pause")},
				Resume:        []string{beacon("resume")},
				Skip:          []string{beacon("skip")},
				Fullscreen:    []string{beacon("fullscreen")},
			},
			Click: vast.ClickSpec{
				ClickThrough:  clickURL,
				ClickTracking: []string{clickURL + "&ev=click-tracking"},
			},
			Pricing: &vast.Pricing{
				Model:    "cpm",
				Currency: "USD",
				Value:    macroCtx.AuctionPrice,
			},
		}

		xmlBytes, err := vast.BuildLinearAd(spec)
		if err != nil {
			reqLog.Error("vast build failed", "error", err)
			http.Error(w, "vast build failed", http.StatusInternalServerError)
			return
		}
		reqLog.Info("vast served", "trace_id", traceID, "duration_s", int(spec.Duration/time.Second))
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(xmlBytes)
	}
}

// indexByte is strings.IndexByte without the strings import in this
// tiny file. Keeps the imports minimal — the only other strings call
// would warrant pulling the package in.
func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}
