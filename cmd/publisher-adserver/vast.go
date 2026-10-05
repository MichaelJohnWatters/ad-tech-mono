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
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/houseads"
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
	Geo              string  `json:"geo"`
	Device           string  `json:"device"`
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
	// AdM carries the winner's ad markup: OpenRTB Native response JSON on
	// channel=native (consumed by native.go), VAST XML on video/audio
	// (OpenRTB §4.3 — parsed + platform trackers injected by adFromWinner,
	// with ${AUCTION_PRICE} already substituted by the exchange).
	AdM string `json:"adm"`
}

// adFromWinner returns the VAST Ad to serve for a video/audio winner. The
// standard path: the winner's bid.adm parses as VAST → serve THAT document's
// ad with the platform's signed trackers injected alongside the buyer's own
// (spec carries the exact tracker set the local build would have used).
// Fallback: adm absent/unparseable/disabled → the legacy local build from
// winner.MediaURL. ok=false when neither path can produce an ad.
func adFromWinner(r *http.Request, winner *sspVideoWinner, spec vast.LinearSpec, consumeAdM bool, secureBase string, reqLog *slog.Logger) (vast.Ad, bool) {
	if consumeAdM && winner.AdM != "" && vast.Sniff(winner.AdM) {
		doc, err := vast.Parse([]byte(winner.AdM))
		if err == nil && len(doc.Ads) > 0 && doc.Ads[0].InLine != nil {
			doc.InjectLinearTrackers(spec.Trackers, spec.ErrorURLs, spec.Click)
			ad := doc.Ads[0]
			ad.Sequence = spec.Sequence
			rehostPlatformMedia(r, &ad, secureBase)
			if len(spec.Verifications) > 0 && ad.InLine.AdVerifications == nil {
				ad.InLine.AdVerifications = vast.AdVerificationsFor(spec.Verifications)
			}
			return ad, true
		}
		reqLog.Warn("winner adm did not parse as VAST — falling back to MediaURL build",
			"error", err, "crid", winner.CreativeID)
	}
	if winner.MediaURL == "" {
		return vast.Ad{}, false
	}
	return vast.SpecToAd(spec), true
}

// rehostPlatformMedia re-hosts PLATFORM-LOCAL MediaFile URIs (localhost,
// cluster service names) onto the secure base on an HTTPS ingress request —
// the adm-path analogue of the MediaURL rewrite. A buyer's external CDN URL
// (dotted public host) is never touched.
func rehostPlatformMedia(r *http.Request, ad *vast.Ad, secureBase string) {
	if ad.InLine == nil {
		return
	}
	for c := range ad.InLine.Creatives.Creatives {
		lin := ad.InLine.Creatives.Creatives[c].Linear
		if lin == nil {
			continue
		}
		for m := range lin.MediaFiles.MediaFiles {
			mf := &lin.MediaFiles.MediaFiles[m]
			if isPlatformLocalURL(mf.URI) {
				mf.URI = rewriteHostIfSecure(r, strings.TrimSpace(mf.URI), secureBase)
			}
		}
	}
}

// isPlatformLocalURL reports whether a media URL points at platform-local
// infrastructure (localhost / docker bridge / an undotted cluster service
// name like minio:9000) as opposed to an external CDN.
func isPlatformLocalURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1" ||
		host == "host.docker.internal" || !strings.Contains(host, ".")
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
func vastHandler(log *slog.Logger, trackerURL, sspURL, secureBase string, omidFn func() (vendor, scriptURL string), stubFn func() bool, houseAdFn houseAdLookup, consumeAdMFn func() bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		traceID := tracing.TraceIDFromContext(ctx)
		if traceID == "" {
			traceID = fmt.Sprintf("vast-%d", time.Now().UnixMilli())
		}
		reqLog := logger.WithContext(log, logger.WithTraceID(ctx, traceID))

		// On an HTTPS ingress request, beacons sign against the secure base and
		// the media URL gets re-hosted to it — otherwise an HTTPS player blocks
		// them as mixed content. The localhost/bridge/e2e path (no
		// X-Forwarded-Proto: https) uses trackerURL / winner.MediaURL unchanged.
		beaconBase := secureTrackerBase(r, trackerURL, secureBase)

		placementID := r.URL.Query().Get("placement_id")
		if placementID == "" {
			placementID = "pl-sport-mpu" // demo default
		}

		// Ad pod (CTV): ?pod=N asks for a pod of up to N ads served back-to-back
		// in one VAST, each from an independent auction with competitive
		// separation (no repeated advertiser within the pod). This is the
		// defining CTV/long-form break shape.
		if podSize := parsePodSize(r.URL.Query().Get("pod")); podSize > 1 {
			if xmlBytes, n := buildPodVAST(ctx, sspURL, beaconBase, secureBase, r, placementID, r.URL.Query(), podSize, omidFn, consumeAdMFn(), reqLog); n > 0 {
				reqLog.Info("video pod served", "requested", podSize, "filled", n)
				adserving.SetOutcome(w, adserving.Outcome{
					Result: adserving.OutcomeFill, Type: "video",
					Reason: fmt.Sprintf("pod %d/%d ads", n, podSize),
				})
				w.Header().Set("Content-Type", "text/xml")
				w.Header().Set("Cache-Control", "no-store")
				w.Write(xmlBytes)
				return
			}
			serveVideoNoBid(w, reqLog, stubFn, houseAdFn, traceID)
			return
		}

		winner, err := fetchVideoWinner(ctx, sspURL, placementID, traceID, r.URL.Query(), nil)
		if err != nil || winner == nil || winner.NoBid || (winner.MediaURL == "" && winner.AdM == "") {
			switch {
			case err != nil:
				reqLog.Warn("video auction failed", "error", err)
			case winner == nil || winner.NoBid:
				reqLog.Info("video auction: no bid")
			default:
				reqLog.Warn("winner had neither adm nor MediaURL", "crid", winner.CreativeID)
			}
			serveVideoNoBid(w, reqLog, stubFn, houseAdFn, traceID)
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
			Channel:      firstNonEmpty(winner.Channel, "video"),
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
			Width:        winner.Width,
			Height:       winner.Height,
			TrackerURL:   beaconBase,
			LandingURL:   landingForDomain(secureBase, winner.AdvertiserDomain),
			URLTTL:       time.Hour,
		}

		// Re-host the creative media URL (http://localhost:8080/...→ secure base)
		// so the HTTPS player loads the <MediaFile>. No-op on non-https requests.
		winner.MediaURL = rewriteHostIfSecure(r, winner.MediaURL, secureBase)
		spec := buildVASTSpec(winner, macroCtx)
		// Open Measurement: embed the configured OMID verification script as
		// <AdVerifications> so measurement vendors can verify/measure the ad.
		// Off unless publisher_adserver.omid_verification_url is set.
		if vendor, scriptURL := omidFn(); scriptURL != "" {
			spec.Verifications = []vast.OMIDVerification{{
				Vendor:         vendor,
				ScriptURL:      scriptURL,
				NotExecutedURL: adserving.AppendClientMacroParams(adserving.BuildVideoEventURL(macroCtx, "omid-not-executed"), "omid-not-executed"),
			}}
		}
		// Standard path: serve the winner's bid.adm VAST with our trackers
		// injected; fallback: local build from MediaURL (adFromWinner).
		ad, ok := adFromWinner(r, winner, spec, consumeAdMFn(), secureBase, reqLog)
		if !ok {
			serveVideoNoBid(w, reqLog, stubFn, houseAdFn, traceID)
			return
		}
		xmlBytes, err := vast.BuildDocument([]vast.Ad{ad})
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
		adserving.SetOutcome(w, adserving.Outcome{
			Result: adserving.OutcomeFill, Type: "video",
			Advertiser: winner.AdvertiserDomain, Price: winner.ClearingPrice,
			Currency: defaultStr2(winner.Currency, "USD"),
			Model:    defaultStr2(winner.BidModel, "cpm"), Deal: winner.DealID,
		})
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
func fetchVideoWinner(ctx context.Context, sspURL, placementID, traceID string, incoming url.Values, excludeAdv []string) (*sspVideoWinner, error) {
	q := forwardSSPQuery(incoming, "video", placementID, "desktop")
	// Competitive separation: exclude advertisers already in the pod via the
	// OpenRTB badv (blocked advertiser domains) the SSP threads into the auction.
	if len(excludeAdv) > 0 {
		q.Set("badv", strings.Join(excludeAdv, ","))
	}
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
func macroCtxForWinner(winner *sspVideoWinner, trackerURL, secureBase string) adserving.MacroContext {
	return adserving.MacroContext{
		AuctionID:    firstNonEmpty(winner.TraceID, winner.CampaignID),
		AuctionPrice: winner.ClearingPrice,
		Channel:      firstNonEmpty(winner.Channel, "video"),
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
		Width:        winner.Width,
		Height:       winner.Height,
		TrackerURL:   trackerURL,
		LandingURL:   landingForDomain(secureBase, winner.AdvertiserDomain),
		URLTTL:       time.Hour,
	}
}

// buildPodVAST runs up to podSize·3 independent auctions and assembles the
// winners into a single sequenced-ad VAST pod. It applies competitive
// separation — an advertiser already present in the pod is skipped — which is
// why it may attempt more auctions than the pod size. Returns the rendered pod
// XML and the number of ads actually filled (0 when nothing filled). Each ad
// carries its own signed trackers, so quartile beacons fire per ad in the pod.
func buildPodVAST(ctx context.Context, sspURL, trackerURL, secureBase string, r *http.Request, placementID string, incoming url.Values, podSize int, omidFn func() (string, string), consumeAdM bool, reqLog *slog.Logger) ([]byte, int) {
	var ads []vast.Ad
	seenAdv := map[string]bool{}
	var excludeAdv []string // picked advertiser domains, threaded to each sub-auction as badv
	maxAttempts := podSize * 3
	for attempt := 0; attempt < maxAttempts && len(ads) < podSize; attempt++ {
		// Each sub-auction excludes the advertisers already in the pod (OpenRTB
		// badv → SSP → exchange → DSP), so competitive separation is enforced at
		// the auction, not just skipped post-hoc — a pod fills distinct
		// advertisers even when one would otherwise win every deterministic bid.
		winner, err := fetchVideoWinner(ctx, sspURL, placementID, "", incoming, excludeAdv)
		if err != nil || winner == nil || winner.NoBid || (winner.MediaURL == "" && winner.AdM == "") {
			continue
		}
		adv := strings.ToLower(winner.AdvertiserDomain)
		if adv != "" && seenAdv[adv] {
			continue // belt-and-braces: badv should already prevent this
		}
		seenAdv[adv] = true
		if adv != "" {
			excludeAdv = append(excludeAdv, adv)
		}

		// Re-host the media URL per pod ad on an HTTPS ingress request (trackerURL
		// here is already the per-request beacon base). No-op otherwise.
		winner.MediaURL = rewriteHostIfSecure(r, winner.MediaURL, secureBase)
		spec := buildVASTSpec(winner, macroCtxForWinner(winner, trackerURL, secureBase))
		spec.Sequence = len(ads) + 1
		if vendor, scriptURL := omidFn(); scriptURL != "" {
			spec.Verifications = []vast.OMIDVerification{{
				Vendor:         vendor,
				ScriptURL:      scriptURL,
				NotExecutedURL: adserving.AppendClientMacroParams(adserving.BuildVideoEventURL(macroCtxForWinner(winner, trackerURL, secureBase), "omid-not-executed"), "omid-not-executed"),
			}}
		}
		// Each pod slot takes the winner's adm when present (standard path),
		// local build otherwise — a pod can mix both shapes.
		ad, ok := adFromWinner(r, winner, spec, consumeAdM, secureBase, reqLog)
		if !ok {
			continue
		}
		ads = append(ads, ad)
	}
	if len(ads) == 0 {
		return nil, 0
	}
	xmlBytes, err := vast.BuildDocument(ads)
	if err != nil {
		reqLog.Error("vast pod build failed", "error", err)
		return nil, 0
	}
	return xmlBytes, len(ads)
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
	// Force the video channel so the viewability beacon carries a signed
	// ch=video and the tracker applies the 2s IAB dwell (vs 1s display).
	macroCtx.Channel = "video"
	impURL := adserving.BuildImpressionURL(macroCtx)
	clickURL := adserving.BuildClickURL(macroCtx)
	// Video viewability beacon → /v1/t/view?ch=video (the player self-measures
	// ≥50%/≥2s and appends dur/pct/area). Distinct from the quartile beacons
	// below, which route to /v1/t/video for the VAST tracking funnel.
	viewURL := adserving.BuildViewabilityURL(macroCtx)
	// Quartile + interaction beacons route through /v1/t/video so the
	// tracker publishes typed VideoEvent on adtech.events.video — not
	// conflated with display viewability on adtech.events.view.
	// The event token is part of the signed URL so a replay with a
	// different event invalidates the HMAC. The IAB bracket macros
	// (cb/ts/pos, + ec on error) ride UNSIGNED after the sig for the
	// player to substitute — the tracker filters them before validation.
	beacon := func(ev string) string {
		return adserving.AppendClientMacroParams(adserving.BuildVideoEventURL(macroCtx, ev), ev)
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
			Viewable:      []string{viewURL},
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

// writeNoFillVAST returns a valid, empty VAST document — the honest "no ad"
// response for a genuine no-bid (default, per the real-data-only rule). A
// compliant player treats an Ad-less VAST as an empty break; the CLI/simulator
// sees zero Impression tags and records a no-fill (not a fake impression).
func writeNoFillVAST(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/xml")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<VAST version="4.2"></VAST>`))
}

// serveVideoNoBid is the no-bid response for the video path. If the house-ad
// fallback is OFF (stub_on_nobid=false) → honest empty VAST (no fake data). If
// ON, look up an ops-configured video house ad and serve ITS markup verbatim
// (the markup is stored as inline VAST XML by staff). If ON but NO video house
// ad is configured, still serve the honest empty VAST — we never invent canned
// content. Seed is trace-derived so weighted rotation is deterministic.
func serveVideoNoBid(w http.ResponseWriter, reqLog *slog.Logger, stubFn func() bool, houseAdFn houseAdLookup, traceID string) {
	if stubFn() && houseAdFn != nil {
		if ad, ok := houseAdFn(houseads.FormatVideo, seedFromTrace(traceID)); ok {
			reqLog.Info("video no-bid: serving configured house ad", "house_ad", ad.ID, "name", ad.Name)
			adserving.SetOutcome(w, adserving.Outcome{Result: adserving.OutcomeHouse, Type: "video", Reason: "no-demand"})
			w.Header().Set("Content-Type", "text/xml")
			w.Header().Set("Cache-Control", "no-store")
			w.Write([]byte(ad.Markup))
			return
		}
		reqLog.Info("video no-bid: house ads on but none configured for video, empty VAST")
	} else {
		reqLog.Info("video no-bid: empty VAST")
	}
	adserving.SetOutcome(w, adserving.Outcome{Result: adserving.OutcomeNoBid, Type: "video", Reason: "no-demand"})
	writeNoFillVAST(w)
}

// landingForDomain maps an advertiser domain to the corresponding
// /dev/landing/{slug} mock URL (served by the gateway) — matches what the
// display flow does via cmd/seed.brandSlugFromDomain. base is the browser-
// reachable gateway base (secureBase = publisher_adserver.public_url_secure,
// https://gateway.<domain>) so the click-through resolves in a browser with NO
// `make demo-forward` bridge — it was hardcoded to http://localhost:8080, which
// only worked behind that bridge.
func landingForDomain(base, domain string) string {
	if base == "" {
		base = "https://gateway.adtech.local"
	}
	base = strings.TrimRight(base, "/")
	if domain == "" {
		return base + "/dev/landing/default"
	}
	slug := domain
	if i := indexByte(slug, '.'); i > 0 {
		slug = slug[:i]
	}
	return base + "/dev/landing/" + slug
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
