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

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/vast"
)

// sspVideoWinner mirrors the cmd/ssp.serveAdResponse video shape — the
// SSP returns this when channel=video so we can build VAST without
// re-running the auction. Subset of the SSP type to keep this file
// self-contained and avoid a cross-cmd import.
type sspVideoWinner struct {
	TraceID          string  `json:"trace_id"`
	NoBid            bool    `json:"nobid"`
	Channel          string  `json:"channel"`
	CreativeID       string  `json:"creative_id"`
	CampaignID       string  `json:"campaign_id"`
	PlacementID      string  `json:"placement_id"`
	PublisherID      string  `json:"publisher_id"`
	AdvertiserID     string  `json:"advertiser_id"`
	AdvertiserDomain string  `json:"advertiser_domain"`
	BidModel         string  `json:"bid_model"`
	Currency         string  `json:"currency"`
	ClearingPrice    float64 `json:"clearing_price"`
	Width            int     `json:"width"`
	Height           int     `json:"height"`
	DurationSeconds  int     `json:"duration_seconds"`
	MediaURL         string  `json:"media_url"`
	DealID           string  `json:"deal_id"`
	// AdM carries native ad markup (OpenRTB Native response JSON) when
	// channel=native; empty for video/audio. Consumed by native.go.
	AdM string `json:"adm"`
}

// vastHandler serves a VAST 4.2 document built from a real auction
// winner. Flow:
//
//  1. Browser/IMA fetches /v1/pubad/video/vast?placement_id=...
//  2. We call SSP /v1/ssp/serve with channel=video — SSP runs the
//     auction (exchange → DSP fan-out), picks the winner, and returns
//     the bid metadata (creative ID, media URL, duration, advertiser
//     domain, etc.) as JSON.
//  3. We construct the signed tracker URLs locally using the winner's
//     identifiers in a MacroContext — same HMAC + exp signing display
//     creatives use, so the existing tracker pipeline covers video
//     unchanged.
//  4. pkg/vast assembles the LinearSpec into VAST XML and we ship it.
//
// On any failure (SSP unreachable, no bid, missing media URL) we fall
// back to a static demo VAST so the simulator never sees a broken
// player. The failure reason gets logged but the response stays valid.
func vastHandler(log *slog.Logger, trackerURL, sspURL string, omidFn func() (vendor, scriptURL string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		traceID := tracing.TraceIDFromContext(ctx)
		if traceID == "" {
			traceID = fmt.Sprintf("vast-%d", time.Now().UnixMilli())
		}
		reqLog := logger.WithContext(log, logger.WithTraceID(ctx, traceID))

		placementID := r.URL.Query().Get("placement_id")
		if placementID == "" {
			placementID = "pl-sport-mpu" // demo default
		}

		// Ad pod (CTV): ?pod=N asks for a pod of up to N ads served back-to-back
		// in one VAST, each from an independent auction with competitive
		// separation (no repeated advertiser within the pod). This is the
		// defining CTV/long-form break shape.
		if podSize := parsePodSize(r.URL.Query().Get("pod")); podSize > 1 {
			if xmlBytes, n := buildPodVAST(ctx, sspURL, trackerURL, placementID, r.URL.Query(), podSize, omidFn, reqLog); n > 0 {
				reqLog.Info("video pod served", "requested", podSize, "filled", n)
				w.Header().Set("Content-Type", "text/xml")
				w.Header().Set("Cache-Control", "no-store")
				w.Write(xmlBytes)
				return
			}
			reqLog.Info("video pod: no fills, serving demo VAST")
			writeStubVAST(w, reqLog, trackerURL, traceID, placementID)
			return
		}

		winner, err := fetchVideoWinner(ctx, sspURL, placementID, traceID, r.URL.Query())
		if err != nil || winner == nil || winner.NoBid || winner.MediaURL == "" {
			if err != nil {
				reqLog.Warn("video auction failed, serving demo VAST", "error", err)
			} else if winner == nil || winner.NoBid {
				reqLog.Info("video auction: no bid, serving demo VAST")
			} else {
				reqLog.Warn("winner had empty MediaURL, serving demo VAST", "crid", winner.CreativeID)
			}
			writeStubVAST(w, reqLog, trackerURL, traceID, placementID)
			return
		}

		// Use the SSP's trace ID once the auction has actually run —
		// keeps all downstream beacons (impression, quartile, click)
		// on the same span tree as the auction itself.
		auctionTrace := winner.TraceID
		if auctionTrace == "" {
			auctionTrace = traceID
		}

		macroCtx := adserving.MacroContext{
			AuctionID:    auctionTrace,
			AuctionPrice: winner.ClearingPrice,
			Currency:     defaultStr2(winner.Currency, "USD"),
			CampaignID:   winner.CampaignID,
			CreativeID:   winner.CreativeID,
			PlacementID:  winner.PlacementID,
			PublisherID:  winner.PublisherID,
			AdvertiserID: winner.AdvertiserID,
			BidModel:     defaultStr2(winner.BidModel, "cpm"),
			DealID:       winner.DealID,
			Width:        winner.Width,
			Height:       winner.Height,
			TrackerURL:   trackerURL,
			LandingURL:   landingForDomain(winner.AdvertiserDomain),
			URLTTL:       time.Hour,
		}

		spec := buildVASTSpec(winner, macroCtx)
		// Open Measurement: embed the configured OMID verification script as
		// <AdVerifications> so measurement vendors can verify/measure the ad.
		// Off unless publisher_adserver.omid_verification_url is set.
		if vendor, scriptURL := omidFn(); scriptURL != "" {
			spec.Verifications = []vast.OMIDVerification{{
				Vendor:         vendor,
				ScriptURL:      scriptURL,
				NotExecutedURL: adserving.BuildVideoEventURL(macroCtx, "omid-not-executed"),
			}}
		}
		xmlBytes, err := vast.BuildLinearAd(spec)
		if err != nil {
			reqLog.Error("vast build failed", "error", err)
			http.Error(w, "vast build failed", http.StatusInternalServerError)
			return
		}
		reqLog.Info("video bid served",
			"trace_id", auctionTrace,
			"creative", winner.CreativeID,
			"advertiser", winner.AdvertiserDomain,
			"duration_s", winner.DurationSeconds,
			"price", winner.ClearingPrice)
		// text/xml without a charset suffix — some VAST parsers (notably
		// older builds of the IMA SDK) refuse application/xml or
		// charset-qualified content types as "unknown ad response".
		w.Header().Set("Content-Type", "text/xml")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(xmlBytes)
	}
}

// fetchVideoWinner calls SSP /v1/ssp/serve?channel=video to run the
// auction and parses the winner JSON. Returns nil on no-bid; returns an
// error only on transport / decode failure (a no-bid is a valid outcome,
// not an error).
func fetchVideoWinner(ctx context.Context, sspURL, placementID, traceID string, incoming url.Values) (*sspVideoWinner, error) {
	q := forwardSSPQuery(incoming, "video", placementID, "desktop")
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
	if winner.NoBid {
		return &winner, nil
	}
	return &winner, nil
}

// parsePodSize parses ?pod=N, clamped to [1,8]. Anything unparseable or ≤1
// means "not a pod" (a single standalone ad).
func parsePodSize(s string) int {
	if s == "" {
		return 1
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 1
	}
	if n > 8 {
		return 8
	}
	return n
}

// macroCtxForWinner builds the MacroContext for one video winner's beacons.
func macroCtxForWinner(winner *sspVideoWinner, trackerURL string) adserving.MacroContext {
	return adserving.MacroContext{
		AuctionID:    firstNonEmpty(winner.TraceID, winner.CampaignID),
		AuctionPrice: winner.ClearingPrice,
		Currency:     defaultStr2(winner.Currency, "USD"),
		CampaignID:   winner.CampaignID,
		CreativeID:   winner.CreativeID,
		PlacementID:  winner.PlacementID,
		PublisherID:  winner.PublisherID,
		AdvertiserID: winner.AdvertiserID,
		BidModel:     defaultStr2(winner.BidModel, "cpm"),
		DealID:       winner.DealID,
		Width:        winner.Width,
		Height:       winner.Height,
		TrackerURL:   trackerURL,
		LandingURL:   landingForDomain(winner.AdvertiserDomain),
		URLTTL:       time.Hour,
	}
}

// buildPodVAST runs up to podSize·3 independent auctions and assembles the
// winners into a single sequenced-ad VAST pod. It applies competitive
// separation — an advertiser already present in the pod is skipped — which is
// why it may attempt more auctions than the pod size. Returns the rendered pod
// XML and the number of ads actually filled (0 when nothing filled). Each ad
// carries its own signed trackers, so quartile beacons fire per ad in the pod.
func buildPodVAST(ctx context.Context, sspURL, trackerURL, placementID string, incoming url.Values, podSize int, omidFn func() (string, string), reqLog *slog.Logger) ([]byte, int) {
	var specs []vast.LinearSpec
	seenAdv := map[string]bool{}
	maxAttempts := podSize * 3
	for attempt := 0; attempt < maxAttempts && len(specs) < podSize; attempt++ {
		winner, err := fetchVideoWinner(ctx, sspURL, placementID, "", incoming)
		if err != nil || winner == nil || winner.NoBid || winner.MediaURL == "" {
			continue
		}
		adv := strings.ToLower(winner.AdvertiserDomain)
		if adv != "" && seenAdv[adv] {
			continue // competitive separation: no repeated advertiser in a pod
		}
		seenAdv[adv] = true

		spec := buildVASTSpec(winner, macroCtxForWinner(winner, trackerURL))
		spec.Sequence = len(specs) + 1
		if vendor, scriptURL := omidFn(); scriptURL != "" {
			spec.Verifications = []vast.OMIDVerification{{
				Vendor:         vendor,
				ScriptURL:      scriptURL,
				NotExecutedURL: adserving.BuildVideoEventURL(macroCtxForWinner(winner, trackerURL), "omid-not-executed"),
			}}
		}
		specs = append(specs, spec)
	}
	if len(specs) == 0 {
		return nil, 0
	}
	xmlBytes, err := vast.BuildPod(specs)
	if err != nil {
		reqLog.Error("vast pod build failed", "error", err)
		return nil, 0
	}
	return xmlBytes, len(specs)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// buildVASTSpec turns the SSP winner + a fully-populated MacroContext
// into a LinearSpec the pkg/vast builder can render. Trackers are
// constructed via the existing macros helpers so the HMAC + exp wiring
// matches display.
func buildVASTSpec(winner *sspVideoWinner, macroCtx adserving.MacroContext) vast.LinearSpec {
	impURL := adserving.BuildImpressionURL(macroCtx)
	clickURL := adserving.BuildClickURL(macroCtx)
	// Quartile + interaction beacons route through /v1/t/video so the
	// tracker publishes typed VideoEvent on adtech.events.video — not
	// conflated with display viewability on adtech.events.view.
	// The event token is part of the signed URL so a replay with a
	// different event invalidates the HMAC.
	beacon := func(ev string) string {
		return adserving.BuildVideoEventURL(macroCtx, ev)
	}

	durationSec := winner.DurationSeconds
	if durationSec <= 0 {
		durationSec = 15
	}
	width := winner.Width
	if width == 0 {
		width = 640
	}
	height := winner.Height
	if height == 0 {
		height = 360
	}

	advTitle := winner.AdvertiserDomain
	if advTitle == "" {
		advTitle = "Video Ad"
	}

	return vast.LinearSpec{
		AdID:       macroCtx.AuctionID,
		AdSystem:   "ad-tech-mono",
		AdTitle:    advTitle,
		Advertiser: winner.AdvertiserDomain,
		Duration:   time.Duration(durationSec) * time.Second,
		MediaFiles: []vast.MediaFile{{
			Delivery: "progressive",
			Type:     "video/mp4",
			Bitrate:  800,
			Width:    width,
			Height:   height,
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
			Fullscreen:    []string{beacon("fullscreen")},
		},
		Click: vast.ClickSpec{
			ClickThrough:  clickURL,
			ClickTracking: []string{clickURL + "&ev=click-tracking"},
		},
		Pricing: &vast.Pricing{
			// VAST 4.x XSD enumerates Pricing/@model as CPM|CPC|CPV|CPA
			// (uppercase). Strict parsers (IMA) reject lowercase values
			// with vast=900 / inner=6 even though most reference VAST
			// samples in the wild are lowercase.
			Model:    strings.ToUpper(defaultStr2(winner.BidModel, "cpm")),
			Currency: defaultStr2(winner.Currency, "USD"),
			Value:    vast.Price(winner.ClearingPrice),
		},
	}
}

// writeStubVAST is the fallback path: same shape as before the
// auction-driven flow, used when SSP is unreachable or there's no bid.
// Keeps the demo player from seeing a 500.
func writeStubVAST(w http.ResponseWriter, reqLog *slog.Logger, trackerURL, traceID, placementID string) {
	macroCtx := adserving.MacroContext{
		AuctionID:    traceID,
		AuctionPrice: 14.50,
		Currency:     "USD",
		CampaignID:   "demo-video-li",
		CreativeID:   "demo-video-cr",
		PlacementID:  placementID,
		PublisherID:  "demo-pub",
		AdvertiserID: "demo-luxauto",
		BidModel:     "cpm",
		Width:        640,
		Height:       360,
		TrackerURL:   trackerURL,
		LandingURL:   "http://localhost:8080/dev/landing/luxauto",
		URLTTL:       time.Hour,
	}
	spec := buildVASTSpec(&sspVideoWinner{
		TraceID:          traceID,
		Channel:          "video",
		CreativeID:       "demo-video-cr",
		CampaignID:       "demo-video-li",
		PlacementID:      placementID,
		AdvertiserDomain: "luxauto.com",
		BidModel:         "cpm",
		Currency:         "USD",
		ClearingPrice:    14.50,
		Width:            640,
		Height:           360,
		DurationSeconds:  15,
		MediaURL:         "http://localhost:8080/v1/media/bigbuckbunny/mp4/h264/360/Big_Buck_Bunny_360_10s_2MB.mp4",
	}, macroCtx)
	xmlBytes, err := vast.BuildLinearAd(spec)
	if err != nil {
		reqLog.Error("stub vast build failed", "error", err)
		http.Error(w, "vast build failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/xml")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(xmlBytes)
}

// landingForDomain maps an advertiser domain to the corresponding
// /dev/landing/{slug} mock URL — matches what the display flow does
// via cmd/seed.brandSlugFromDomain. Inlined here so this file stays
// self-contained.
func landingForDomain(domain string) string {
	if domain == "" {
		return "http://localhost:8080/dev/landing/default"
	}
	slug := domain
	if i := indexByte(slug, '.'); i > 0 {
		slug = slug[:i]
	}
	return "http://localhost:8080/dev/landing/" + slug
}

// indexByte is strings.IndexByte without the strings import in this
// file. Keeps the imports minimal.
func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func defaultStr2(s, dflt string) string {
	if s == "" {
		return dflt
	}
	return s
}
