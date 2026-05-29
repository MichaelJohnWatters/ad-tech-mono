// Package adserving provides macro substitution and pixel URL generation
// for the ad server. Macros are placeholders like ${AUCTION_PRICE} in
// creative HTML that get replaced with real values at serve time.
package adserving

import (
	"fmt"
	"math/rand"
	"net/url"
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
	SiteDomain   string
	AppBundle    string
	Width        int
	Height       int
	UserAgent    string
	IP           string
	TrackerURL   string // base URL for tracker service
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
	params.Set("sig", "TODO") // HMAC signature
	return ctx.TrackerURL + "/v1/t/imp?" + params.Encode()
}

// BuildClickURL builds the click tracking URL.
func BuildClickURL(ctx MacroContext) string {
	params := url.Values{}
	params.Set("tid", ctx.AuctionID)
	params.Set("cid", ctx.CampaignID)
	params.Set("crid", ctx.CreativeID)
	params.Set("pid", ctx.PlacementID)
	params.Set("pubid", ctx.PublisherID)
	params.Set("sig", "TODO")
	return ctx.TrackerURL + "/v1/t/click?" + params.Encode()
}

// BuildViewabilityURL builds the viewability beacon URL.
func BuildViewabilityURL(ctx MacroContext) string {
	params := url.Values{}
	params.Set("tid", ctx.AuctionID)
	params.Set("cid", ctx.CampaignID)
	params.Set("pid", ctx.PlacementID)
	params.Set("pubid", ctx.PublisherID)
	params.Set("sig", "TODO")
	return ctx.TrackerURL + "/v1/t/view?" + params.Encode()
}
