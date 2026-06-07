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
	UserAgent    string
	IP           string
	TrackerURL   string // base URL for tracker service
	LandingURL   string // baked into the signed click URL as redir= so the tracker can 302 to the advertiser page after recording the click
	URLTTL       time.Duration // signed-URL freshness window; baked in as exp=<unix-ts> param and covered by the HMAC. 0 = no exp (URL never expires, replay-able forever once HMAC is stolen).
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
	setExp(params, ctx.URLTTL)
	rawURL := ctx.TrackerURL + "/v1/t/imp?" + params.Encode()
	return SignURL(rawURL, DefaultSigningKey)
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
	if ctx.LandingURL != "" {
		params.Set("redir", ctx.LandingURL)
	}
	setExp(params, ctx.URLTTL)
	rawURL := ctx.TrackerURL + "/v1/t/click?" + params.Encode()
	return SignURL(rawURL, DefaultSigningKey)
}

// BuildViewabilityURL builds the viewability beacon URL.
func BuildViewabilityURL(ctx MacroContext) string {
	params := url.Values{}
	params.Set("tid", ctx.AuctionID)
	params.Set("cid", ctx.CampaignID)
	params.Set("pid", ctx.PlacementID)
	params.Set("pubid", ctx.PublisherID)
	setExp(params, ctx.URLTTL)
	rawURL := ctx.TrackerURL + "/v1/t/view?" + params.Encode()
	return SignURL(rawURL, DefaultSigningKey)
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
	if event != "" {
		params.Set("event", event)
	}
	setExp(params, ctx.URLTTL)
	rawURL := ctx.TrackerURL + "/v1/t/video?" + params.Encode()
	return SignURL(rawURL, DefaultSigningKey)
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
	if event != "" {
		params.Set("event", event)
	}
	setExp(params, ctx.URLTTL)
	rawURL := ctx.TrackerURL + "/v1/t/audio?" + params.Encode()
	return SignURL(rawURL, DefaultSigningKey)
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
