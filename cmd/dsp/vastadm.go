package main

import (
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/vast"
)

// buildMediaAdM builds the standard OpenRTB §4.3 ad markup for a video/audio
// bid: a VAST 4.2 InLine document carrying the creative's MediaFile. Called
// ONCE on the final winning candidate after the campaign loop (hot-path iron
// rule — never per-campaign), and it's a pure in-memory marshal: no I/O.
//
// DECISION: the DSP's InLine carries NO tracking beacons and NO Pricing.
// The platform's single source of truth for cost is the AuctionWinEvent and
// the DSP already receives exchange win notices (nurl); DSP-side beacons
// would be cluster-internal URLs a browser can't reach. The standard "buyer
// tracks its own delivery" slot is exercised end-to-end by cmd/extbidder,
// whose adm carries its own ${AUCTION_PRICE} impression. The exchange-side
// consumer (publisher-adserver) injects the platform's signed trackers into
// this document at serve time, preserving whatever the buyer put in.
//
// MediaURL stays on the bid as the documented back-compat extension — the
// publisher-adserver's fallback when adm is absent/unparseable.
func buildMediaAdM(c *models.Campaign, bid *openrtb.BidObj, format string) (string, bool) {
	if bid.MediaURL == "" {
		return "", false
	}
	dur := bid.Dur
	if dur <= 0 {
		dur = 15
	}
	mf := vast.MediaFile{
		Delivery: "progressive",
		Type:     mediaMIME(bid.MediaURL, format),
		URI:      bid.MediaURL,
	}
	if format != "audio" {
		mf.Width, mf.Height = bid.W, bid.H
	}
	title := c.Name
	if title == "" {
		title = c.CreativeDomain
	}
	xmlBytes, err := vast.BuildLinearAd(vast.LinearSpec{
		AdID:          bid.CrID,
		AdSystem:      "ad-tech-mono-dsp",
		AdTitle:       title,
		Advertiser:    c.CreativeDomain,
		Duration:      time.Duration(dur) * time.Second,
		MediaFiles:    []vast.MediaFile{mf},
		UniversalAdID: vast.UniversalAdID{IDRegistry: "ad-tech-mono", Value: bid.CrID},
	})
	if err != nil {
		return "", false
	}
	return string(xmlBytes), true
}

// vastTagForBid returns the selected creative's third-party VAST tag URL
// ("" for hosted-asset creatives). Looked up by the bid's creative id on the
// final winner only — same off-hot-loop discipline as buildMediaAdM.
func vastTagForBid(c *models.Campaign, creativeID string) string {
	for i := range c.Creatives {
		if c.Creatives[i].ID == creativeID {
			return c.Creatives[i].VASTTagURL
		}
	}
	return ""
}

// buildWrapperAdM builds the Wrapper bid markup for a third-party VAST tag
// creative (VAST 4.2 §3.19): VASTAdTagURI = the tag, NO DSP-side trackers
// (same decision as buildMediaAdM — the platform injects its signed set at
// wrapper level in the publisher-adserver). The PLAYER resolves the chain;
// no server in this platform fetches the tag.
func buildWrapperAdM(bid *openrtb.BidObj, tagURL string) (string, bool) {
	xmlBytes, err := vast.BuildWrapperAd(vast.WrapperSpec{
		AdID:         bid.CrID,
		AdSystem:     "ad-tech-mono-dsp",
		VASTAdTagURI: tagURL,
	})
	if err != nil {
		return "", false
	}
	return string(xmlBytes), true
}

// mediaMIME infers the MediaFile MIME from the URL extension, defaulting to
// the format's canonical type (video/mp4, audio/mpeg).
func mediaMIME(mediaURL, format string) string {
	u := strings.ToLower(mediaURL)
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	switch {
	case strings.HasSuffix(u, ".webm"):
		return "video/webm"
	case strings.HasSuffix(u, ".m4a"), strings.HasSuffix(u, ".aac"):
		return "audio/mp4"
	case strings.HasSuffix(u, ".mp3"):
		return "audio/mpeg"
	case strings.HasSuffix(u, ".ogg"):
		return "audio/ogg"
	}
	if format == "audio" {
		return "audio/mpeg"
	}
	return "video/mp4"
}
