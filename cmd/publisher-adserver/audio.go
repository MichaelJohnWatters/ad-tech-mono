package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/houseads"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/vast"
)

// audioHandler serves a VAST 4.2 document carrying an audio MediaFile, built
// from a real audio auction winner. It is the audio analogue of vastHandler:
//
//  1. A podcast / streaming-radio player fetches /v1/pubad/audio?placement_id=…
//  2. We call SSP /v1/ssp/serve with channel=audio — the SSP runs the auction
//     and returns the winner's media URL + duration (it stays format-agnostic;
//     the VAST/DAAST assembly lives here).
//  3. We render a VAST 4.2 <Linear> with an audio/mpeg MediaFile (no width/
//     height) and quartile beacons routed through /v1/t/audio, so the tracker
//     publishes typed AudioEvent rather than video/display events.
//
// Modern audio ad serving uses VAST 4.x audio MediaFiles rather than the
// deprecated DAAST document, so we reuse pkg/vast — the only difference from
// video is the MediaFile MIME type and the beacon endpoint.
func audioHandler(log *slog.Logger, trackerURL, sspURL string, stubFn func() bool, houseAdFn houseAdLookup) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		traceID := tracing.TraceIDFromContext(ctx)
		if traceID == "" {
			traceID = fmt.Sprintf("audio-%d", time.Now().UnixMilli())
		}
		reqLog := logger.WithContext(log, logger.WithTraceID(ctx, traceID))

		placementID := r.URL.Query().Get("placement_id")
		if placementID == "" {
			placementID = "pl-sim-audio" // demo default
		}

		winner, err := fetchAudioWinner(ctx, sspURL, placementID, traceID, r.URL.Query())
		if err != nil || winner == nil || winner.NoBid || winner.MediaURL == "" {
			switch {
			case err != nil:
				reqLog.Warn("audio auction failed", "error", err)
			case winner == nil || winner.NoBid:
				reqLog.Info("audio auction: no bid")
			default:
				reqLog.Warn("winner had empty MediaURL", "crid", winner.CreativeID)
			}
			serveAudioNoBid(w, reqLog, stubFn, houseAdFn, traceID)
			return
		}

		auctionTrace := winner.TraceID
		if auctionTrace == "" {
			auctionTrace = traceID
		}

		macroCtx := adserving.MacroContext{
			AuctionID:    auctionTrace,
			AuctionPrice: winner.ClearingPrice,
			Channel:      firstNonEmpty(winner.Channel, "audio"),
			Geo:          winner.Geo,
			Device:       winner.Device,
			Currency:     defaultStr2(winner.Currency, "USD"),
			CampaignID:   winner.CampaignID,
			CreativeID:   winner.CreativeID,
			PlacementID:  winner.PlacementID,
			PublisherID:  winner.PublisherID,
			AdvertiserID: winner.AdvertiserID,
			BidModel:     defaultStr2(winner.BidModel, "cpm"),
			DealID:       winner.DealID,
			TrackerURL:   trackerURL,
			LandingURL:   landingForDomain(winner.AdvertiserDomain),
			URLTTL:       time.Hour,
		}

		spec := buildAudioVASTSpec(winner, macroCtx)
		xmlBytes, err := vast.BuildLinearAd(spec)
		if err != nil {
			reqLog.Error("audio vast build failed", "error", err)
			http.Error(w, "audio vast build failed", http.StatusInternalServerError)
			return
		}
		reqLog.Info("audio bid served",
			"trace_id", auctionTrace,
			"creative", winner.CreativeID,
			"advertiser", winner.AdvertiserDomain,
			"duration_s", winner.DurationSeconds,
			"price", winner.ClearingPrice)
		w.Header().Set("Content-Type", "text/xml")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(xmlBytes)
	}
}

// fetchAudioWinner calls SSP /v1/ssp/serve?channel=audio. Returns nil on no-bid;
// error only on transport/decode failure. Reuses sspVideoWinner — the SSP
// returns the same shape for video and audio (see cmd/ssp serveAdResponse).
func fetchAudioWinner(ctx context.Context, sspURL, placementID, traceID string, incoming url.Values) (*sspVideoWinner, error) {
	q := forwardSSPQuery(incoming, "audio", placementID, "mobile")
	target := sspURL + routes.SSPServe + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("build ssp request: %w", err)
	}
	tracing.InjectHTTP(ctx, req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ssp call: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ssp body read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ssp returned %d: %s", resp.StatusCode, string(body))
	}
	var winner sspVideoWinner
	if err := json.Unmarshal(body, &winner); err != nil {
		return nil, fmt.Errorf("ssp decode: %w", err)
	}
	return &winner, nil
}

// buildAudioVASTSpec turns the SSP winner + MacroContext into a LinearSpec with
// an audio MediaFile. Quartile/interaction beacons route through /v1/t/audio
// (BuildAudioEventURL) so downstream picks up typed AudioEvent. Audio has no
// width/height, so the MediaFile omits them.
func buildAudioVASTSpec(winner *sspVideoWinner, macroCtx adserving.MacroContext) vast.LinearSpec {
	impURL := adserving.BuildImpressionURL(macroCtx)
	clickURL := adserving.BuildClickURL(macroCtx)
	beacon := func(ev string) string {
		return adserving.BuildAudioEventURL(macroCtx, ev)
	}

	durationSec := winner.DurationSeconds
	if durationSec <= 0 {
		durationSec = 30
	}
	advTitle := winner.AdvertiserDomain
	if advTitle == "" {
		advTitle = "Audio Ad"
	}

	return vast.LinearSpec{
		AdID:       macroCtx.AuctionID,
		AdSystem:   "ad-tech-mono",
		AdTitle:    advTitle,
		Advertiser: winner.AdvertiserDomain,
		Duration:   time.Duration(durationSec) * time.Second,
		MediaFiles: []vast.MediaFile{{
			Delivery: "progressive",
			Type:     "audio/mpeg",
			Bitrate:  128,
			URI:      winner.MediaURL,
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
		},
		ErrorURLs: []string{beacon("error")},
		Click: vast.ClickSpec{
			ClickThrough: clickURL,
			ClickTracking: []string{
				// Re-sign: appending a param to an already-signed URL
				// invalidates the HMAC (every VAST/DAAST click tracker
				// failed verification for as long as this append existed —
				// unnoticed because tracker.signature_validation was off).
				adserving.SignURL(clickURL+"&ev=click-tracking", adserving.ActiveSigningKey()),
			},
		},
		Pricing: &vast.Pricing{
			Model:    strings.ToUpper(defaultStr2(macroCtx.BidModel, "cpm")),
			Currency: defaultStr2(macroCtx.Currency, "USD"),
			Value:    vast.Price(macroCtx.AuctionPrice),
		},
	}
}

// serveAudioNoBid is the audio-path no-bid response. Mirrors serveVideoNoBid:
// honest empty VAST unless the house-ad fallback is on AND an audio house ad is
// configured, in which case its markup (inline VAST XML) is served verbatim.
func serveAudioNoBid(w http.ResponseWriter, reqLog *slog.Logger, stubFn func() bool, houseAdFn houseAdLookup, traceID string) {
	if stubFn() && houseAdFn != nil {
		if ad, ok := houseAdFn(houseads.FormatAudio, seedFromTrace(traceID)); ok {
			reqLog.Info("audio no-bid: serving configured house ad", "house_ad", ad.ID, "name", ad.Name)
			w.Header().Set("Content-Type", "text/xml")
			w.Header().Set("Cache-Control", "no-store")
			w.Write([]byte(ad.Markup))
			return
		}
		reqLog.Info("audio no-bid: house ads on but none configured for audio, empty VAST")
	} else {
		reqLog.Info("audio no-bid: empty VAST")
	}
	writeNoFillVAST(w)
}
