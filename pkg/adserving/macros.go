// Package adserving provides macro substitution and pixel URL generation
// for the ad server. Macros are placeholders like ${AUCTION_PRICE} in
// creative HTML that get replaced with real values at serve time.
package adserving

import (
	"fmt"
	"math/rand"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ara"
)

// MacroContext holds the values available for macro substitution.
type MacroContext struct {
	AuctionID    string  // trace_id
	AuctionPrice float64 // clearing price
	Currency     string  // ISO 4217
	CampaignID   string  // line_item.id
	CreativeID   string
	PlacementID  string
	PublisherID  string
	AdvertiserID string
	IOId         string // insertion order
	DealID       string
	BidModel     string // cpm, cpc, cpa, vcpm, cpcv — propagated on tracker URLs so billing engine can route reserve vs bill-immediately
	SiteDomain   string
	AppBundle    string
	Width        int
	Height       int
	Geo          string // request geo (country) — baked into tracker beacons for analytics
	Device       string // request device type — baked into tracker beacons for analytics
	// UserID is the hashed user id, present ONLY when the serve chain carried
	// consent (models.ServeRequest.UserID: "empty = no consent"). Baked into
	// tracker beacons as uid= so interaction events can feed the consent-gated
	// behaviour_signals table; its presence IS the consent signal downstream.
	UserID string
	// Household is the SSP-derived salted-IP household id ("hh:…"). Baked onto
	// consented beacons as hh= so behaviour_signals carries it — the fallback key
	// view-through attribution matches when the exact user id doesn't line up
	// (cross-device / CTV). Consent-coupled: only ridden alongside a consented uid.
	Household  string
	Channel    string // display|video|native|audio — baked into tracker beacons so analytics label the right channel
	UserAgent  string
	IP         string
	TrackerURL string        // base URL for tracker service
	LandingURL string        // baked into the signed click URL as redir= so the tracker can 302 to the advertiser page after recording the click
	URLTTL     time.Duration // signed-URL freshness window; baked in as exp=<unix-ts> param and covered by the HMAC. 0 = no exp (URL never expires, replay-able forever once HMAC is stolen).
}

// SubstituteMacros replaces all ${...} macros in the input string.
func SubstituteMacros(input string, ctx MacroContext) string {
	now := time.Now()
	cachebuster := fmt.Sprintf("%d", rand.Int63())

	replacements := map[string]string{
		"${AUCTION_PRICE}": fmt.Sprintf("%.2f", ctx.AuctionPrice),
		"${AUCTION_ID}":    ctx.AuctionID,
		"${CREATIVE_ID}":   ctx.CreativeID,
		"${CAMPAIGN_ID}":   ctx.CampaignID,
		"${LINE_ITEM_ID}":  ctx.CampaignID,
		"${IO_ID}":         ctx.IOId,
		"${ADVERTISER_ID}": ctx.AdvertiserID,
		"${PLACEMENT_ID}":  ctx.PlacementID,
		"${PUBLISHER_ID}":  ctx.PublisherID,
		"${SITE_DOMAIN}":   ctx.SiteDomain,
		"${APP_BUNDLE}":    ctx.AppBundle,
		"${WIDTH}":         fmt.Sprintf("%d", ctx.Width),
		"${HEIGHT}":        fmt.Sprintf("%d", ctx.Height),
		"${CURRENCY}":      ctx.Currency,
		"${DEAL_ID}":       ctx.DealID,
		"${TIMESTAMP}":     fmt.Sprintf("%d", now.Unix()),
		"${CACHEBUSTER}":   cachebuster,
		"${USER_AGENT}":    url.QueryEscape(ctx.UserAgent),
		"${IP}":            ctx.IP,
	}

	// Click URL with all context baked in
	clickURL := BuildClickURL(ctx)
	replacements["${CLICK_URL}"] = clickURL
	replacements["${CLICK_URL_ENC}"] = url.QueryEscape(clickURL)

	result := input
	for macro, value := range replacements {
		result = strings.ReplaceAll(result, macro, value)
	}
	return result
}

// BuildImpressionURL builds the full impression pixel URL with all context.
func BuildImpressionURL(ctx MacroContext) string {
	params := url.Values{}
	params.Set("tid", ctx.AuctionID)
	params.Set("cid", ctx.CampaignID)
	params.Set("crid", ctx.CreativeID)
	params.Set("pid", ctx.PlacementID)
	params.Set("pubid", ctx.PublisherID)
	params.Set("advid", ctx.AdvertiserID)
	params.Set("price", fmt.Sprintf("%.4f", ctx.AuctionPrice))
	params.Set("cur", ctx.Currency)
	params.Set("w", fmt.Sprintf("%d", ctx.Width))
	params.Set("h", fmt.Sprintf("%d", ctx.Height))
	if ctx.DealID != "" {
		params.Set("deal", ctx.DealID)
	}
	if ctx.BidModel != "" {
		params.Set("bm", ctx.BidModel)
	}
	setGeoDevice(params, ctx)
	setExp(params, ctx.URLTTL)
	rawURL := ctx.TrackerURL + "/v1/t/imp?" + params.Encode()
	return SignURL(rawURL, ActiveSigningKey())
}

// BuildClickURL builds the click tracking URL. LandingURL (when set) is
// baked in as the redir= param BEFORE signing so the tracker can 302 to
// the advertiser page after recording the click without invalidating the
// HMAC. Creative HTML therefore uses bare ${CLICK_URL} (no concatenation)
// and the tracker handles the redirect.
func BuildClickURL(ctx MacroContext) string {
	params := url.Values{}
	params.Set("tid", ctx.AuctionID)
	params.Set("cid", ctx.CampaignID)
	params.Set("crid", ctx.CreativeID)
	params.Set("pid", ctx.PlacementID)
	params.Set("pubid", ctx.PublisherID)
	// advid rides signed (same as the impression pixel) so the tracker can stamp
	// account_id on the click event — without it, clicks land in ClickHouse with
	// an empty account_id and every advertiser's tenant-scoped "Clicks 7d" reads 0.
	params.Set("advid", ctx.AdvertiserID)
	if ctx.LandingURL != "" {
		params.Set("redir", ctx.LandingURL)
	}
	setGeoDevice(params, ctx)
	setExp(params, ctx.URLTTL)
	rawURL := ctx.TrackerURL + "/v1/t/click?" + params.Encode()
	return SignURL(rawURL, ActiveSigningKey())
}

// BuildARASourceURL builds the SIGNED Privacy Sandbox ARA source-registration
// beacon the browser fetches via attributionsrc (see pkg/ara + the tracker's
// /v1/t/ara/src). Returns "" when there's no landing URL (no ARA destination) or
// no advertiser. `dest` is the advertiser SITE (scheme + registrable domain) — the
// thing ARA matches a conversion's destination against — and rides signed so the
// tracker can't be handed a forged advid.
func BuildARASourceURL(ctx MacroContext) string {
	dest := araDestination(ctx.LandingURL)
	if dest == "" || ctx.AdvertiserID == "" {
		return ""
	}
	params := url.Values{}
	params.Set("advid", ctx.AdvertiserID)
	params.Set("dest", dest)
	if ctx.CampaignID != "" {
		params.Set("cid", ctx.CampaignID)
	}
	setExp(params, ctx.URLTTL)
	rawURL := ctx.TrackerURL + "/v1/t/ara/src?" + params.Encode()
	return SignURL(rawURL, ActiveSigningKey())
}

// araDestination returns the ARA destination — scheme + registrable domain — for
// a landing URL (https://shop.acme.co.uk/x → https://acme.co.uk). Empty when the
// URL has no host. The tracker re-derives the same value from the beacon's dest=
// param at registration, so this MUST stay the single canonical reduction:
// ara.NormalizeDestination.
func araDestination(landingURL string) string {
	dest, ok := ara.NormalizeDestination(landingURL)
	if !ok {
		return ""
	}
	return dest
}

// BuildViewabilityURL builds the viewability beacon URL.
func BuildViewabilityURL(ctx MacroContext) string {
	params := url.Values{}
	params.Set("tid", ctx.AuctionID)
	params.Set("cid", ctx.CampaignID)
	params.Set("pid", ctx.PlacementID)
	params.Set("pubid", ctx.PublisherID)
	if ctx.UserID != "" {
		params.Set("uid", ctx.UserID)
		if ctx.Household != "" {
			params.Set("hh", ctx.Household)
		}
	}
	// Channel rides SIGNED (the server knows it at serve time) so the tracker
	// applies the right IAB dwell — 2s for video vs 1s for display — and it
	// can't be downgraded by tampering. Omitted for display (the default).
	if ctx.Channel == "video" {
		params.Set("ch", ctx.Channel)
	}
	setExp(params, ctx.URLTTL)
	rawURL := ctx.TrackerURL + "/v1/t/view?" + params.Encode()
	return SignURL(rawURL, ActiveSigningKey())
}

// BuildVideoEventURL builds a signed URL for a single video player
// event (start, firstQuartile, midpoint, thirdQuartile, complete, mute,
// pause, resume, skip, fullscreen). Mirrors BuildViewabilityURL's
// param set so the tracker handler can read trace_id + campaign + etc.
// for attribution, but routes to /v1/t/video so the downstream picks
// up a typed VideoEvent on adtech.events.video instead of conflating
// with display viewability events.
//
// event is part of the signed param set so a re-signed URL with a
// different event token would invalidate. Empty event → omitted; the
// tracker handler treats that as a generic video event with empty
// EventType, which is what the existing /v1/t/video?event= contract
// already supports.
func BuildVideoEventURL(ctx MacroContext, event string) string {
	params := url.Values{}
	params.Set("tid", ctx.AuctionID)
	params.Set("cid", ctx.CampaignID)
	params.Set("crid", ctx.CreativeID)
	params.Set("pid", ctx.PlacementID)
	params.Set("pubid", ctx.PublisherID)
	// advid rides signed (same as the impression pixel) so the tracker can
	// stamp account_id onto the media event — advertiser-tenant scoping for
	// quartile reporting works exactly like impressions.
	params.Set("advid", ctx.AdvertiserID)
	if event != "" {
		params.Set("event", event)
	}
	setExp(params, ctx.URLTTL)
	rawURL := ctx.TrackerURL + "/v1/t/video?" + params.Encode()
	return SignURL(rawURL, ActiveSigningKey())
}

// BuildAudioEventURL is the audio analogue — same shape, different
// path. Used for podcast / streaming-radio quartile + interaction
// beacons in the audio sim flow.
func BuildAudioEventURL(ctx MacroContext, event string) string {
	params := url.Values{}
	params.Set("tid", ctx.AuctionID)
	params.Set("cid", ctx.CampaignID)
	params.Set("crid", ctx.CreativeID)
	params.Set("pid", ctx.PlacementID)
	params.Set("pubid", ctx.PublisherID)
	params.Set("advid", ctx.AdvertiserID) // signed, same rationale as video
	if event != "" {
		params.Set("event", event)
	}
	setExp(params, ctx.URLTTL)
	rawURL := ctx.TrackerURL + "/v1/t/audio?" + params.Encode()
	return SignURL(rawURL, ActiveSigningKey())
}

// AppendClientMacroParams appends UNSIGNED, player-substituted IAB
// bracket-macro params (VAST 4.2 §6) to an already-signed media beacon
// URL: cb=[CACHEBUSTING], ts=[TIMESTAMP], pos=[ADPLAYHEAD], plus
// ec=[ERRORCODE] on error beacons. Appended AFTER SignURL on the
// viewability-beacon precedent (dur/pct/area): the tracker excludes
// exactly these params from signature validation (mediaSigParams in
// cmd/tracker) — event and every identity param stay inside the HMAC.
//
// The tokens are appended as raw literals, NOT url.Values-encoded:
// players substitute by literal text replacement, and an encoded
// %5BERRORCODE%5D is invisible to them. Only call this on PLAYER-facing
// URLs (publisher-adserver VAST/audio output) — server-fired beacons
// (SSAI stitcher, simulator --direct) must keep macro-free URLs or the
// tokens arrive unsubstituted.
func AppendClientMacroParams(signedURL, event string) string {
	sep := "&"
	if !strings.Contains(signedURL, "?") {
		sep = "?"
	}
	out := signedURL + sep + "cb=[CACHEBUSTING]&ts=[TIMESTAMP]&pos=[ADPLAYHEAD]"
	if event == "error" {
		out += "&ec=[ERRORCODE]"
	}
	return out
}

// setExp adds an exp=<unix-seconds> param to a tracker-URL param set
// when ttl > 0. Covered by the HMAC because it's added before SignURL,
// so any rewrite invalidates the sig. The tracker rejects requests with
// now > exp when tracker.exp_validation is enabled.
func setExp(params url.Values, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	params.Set("exp", strconv.FormatInt(time.Now().Add(ttl).Unix(), 10))
}

// setGeoDevice adds geo/dev params to a tracker-URL param set when present.
// Added before SignURL so they're covered by the HMAC. The tracker reads
// these onto the impression/click analytics events (geo/device columns).
func setGeoDevice(params url.Values, ctx MacroContext) {
	if ctx.Geo != "" {
		params.Set("geo", ctx.Geo)
	}
	if ctx.Device != "" {
		params.Set("dev", ctx.Device)
	}
	if ctx.Channel != "" {
		params.Set("ch", ctx.Channel)
	}
	// Consent-gated by construction: UserID is only non-empty when the serve
	// request carried a consented user (see MacroContext.UserID). The household
	// rides alongside it (never on an unconsented beacon).
	if ctx.UserID != "" {
		params.Set("uid", ctx.UserID)
		if ctx.Household != "" {
			params.Set("hh", ctx.Household)
		}
	}
}
