// Package vast generates VAST 4.2 XML for video and audio creatives.
//
// VAST (Video Ad Serving Template) is the IAB-standard XML protocol video
// players use to request and play ads. VAST 4.x extended the format to
// also cover audio (replacing the deprecated DAAST 1.0), so the same
// builder handles both — the only difference is whether MediaFile MIMEs
// are video/* or audio/*, and audio creatives skip the width/height
// attributes.
//
// Spec reference: https://iabtechlab.com/wp-content/uploads/2019/06/VAST_4.2_final_june26.pdf
//
// This package's only public surface is BuildLinearAd; the types below
// are exported so callers that need to hand-craft a custom VAST shape
// (in-banner outstream, wrapper redirects, companion-only ads) can
// emit the same struct tree directly.
package vast

import (
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Version is the VAST version string emitted in the root element.
// 4.2 is the latest stable as of 2024 and is universally supported by
// the major players (Google IMA, JW Player, hls.js, etc.).
const Version = "4.2"

// VAST is the root element of a VAST document. xmlns and the XSD
// reference are intentionally omitted — players accept the unqualified
// form and most VAST samples in the wild don't include them.
type VAST struct {
	XMLName xml.Name `xml:"VAST"`
	Version string   `xml:"version,attr"`
	Ads     []Ad     `xml:"Ad"`
}

// Ad represents one ad in the response. Multiple Ads in a single VAST
// document is how ad pods are served — the player iterates by Sequence
// and plays each in order.
type Ad struct {
	ID       string  `xml:"id,attr"`
	Sequence int     `xml:"sequence,attr,omitempty"`
	InLine   *InLine `xml:"InLine,omitempty"`
	// Wrapper is the other choice — a redirect to a downstream VAST URL.
	// Not used today (we always serve InLine); added when the platform
	// starts proxying third-party VAST tags through our exchange.
	Wrapper *Wrapper `xml:"Wrapper,omitempty"`
}

// InLine is a fully-self-contained ad: media files + tracking URLs are
// embedded directly. The other shape is Wrapper (a VASTAdTagURI redirect
// to another VAST document), which we don't generate today.
//
// Field order matches the IAB VAST 4.x XSD: AdSystem → AdTitle →
// Impression → Description → Advertiser → Pricing → Creatives. Strict
// parsers (notably the IMA SDK) reject documents that emit these in
// a different order — Impression after Pricing produces an
// AD_LOAD_ERROR with no other diagnostic.
type InLine struct {
	AdSystem    AdSystem     `xml:"AdSystem"`
	AdTitle     string       `xml:"AdTitle"`
	Impressions []Impression `xml:"Impression"`
	Description string       `xml:"Description,omitempty"`
	Advertiser  string       `xml:"Advertiser,omitempty"`
	Pricing     *Pricing     `xml:"Pricing,omitempty"`
	Creatives   Creatives    `xml:"Creatives"`
}

// Wrapper redirects the player to another VAST document. Reserved for
// future use when we proxy third-party VAST through our exchange.
type Wrapper struct {
	AdSystem     AdSystem     `xml:"AdSystem"`
	VASTAdTagURI string       `xml:"VASTAdTagURI"`
	Impressions  []Impression `xml:"Impression,omitempty"`
}

// AdSystem identifies the platform that generated the VAST. Players
// surface this in debug overlays so publishers can attribute traffic.
type AdSystem struct {
	Name    string `xml:",chardata"`
	Version string `xml:"version,attr,omitempty"`
}

// Pricing is the floor / clearing price expressed inside the VAST. Most
// players ignore it for actual billing (the exchange's win notice is
// authoritative) but it's part of the spec and useful for debug.
//
// Value is stored as a typed float so callers don't need to format the
// price themselves, but the XML marshaller rounds to 4 decimal places
// to keep the printed value clean (Go's default %v would emit
// floating-point artefacts like "6.864000000000001", which some VAST
// parsers — including the IMA SDK — reject as malformed).
type Pricing struct {
	Model    string `xml:"model,attr"` // "cpm", "cpc", "cpe", "cpv"
	Currency string `xml:"currency,attr"`
	Value    Price  `xml:",chardata"`
}

// Price is a float64 alias that rounds to 4 decimals on text marshal
// so the VAST output doesn't carry the trailing precision artefacts
// Go's default float formatting produces.
type Price float64

func (p Price) MarshalText() ([]byte, error) {
	// 4 decimals is the IAB-recommended precision for CPM display
	// pricing — enough to express sub-cent values while keeping the
	// output free of floating-point artefacts like 6.864000000000001.
	return []byte(strconv.FormatFloat(float64(p), 'f', 4, 64)), nil
}

func (p *Price) UnmarshalText(text []byte) error {
	f, err := strconv.ParseFloat(string(text), 64)
	if err != nil {
		return err
	}
	*p = Price(f)
	return nil
}

// Impression is a tracking URL the player pings exactly once when the
// ad is first rendered. Multiple Impressions can be listed — useful
// when a wrapper layers its own beacon on top of a downstream VAST.
type Impression struct {
	ID  string `xml:"id,attr,omitempty"`
	URI string `xml:",cdata"`
}

// Creatives is the wrapper element around one or more Creative entries.
// Multiple Creative entries describe (a) a linear video/audio creative
// + (b) zero or more companions / non-linear overlays.
type Creatives struct {
	Creatives []Creative `xml:"Creative"`
}

// Creative is one playable element. ID is platform-internal; the
// player echoes it back on tracking calls so we can attribute.
type Creative struct {
	ID            string         `xml:"id,attr,omitempty"`
	Sequence      int            `xml:"sequence,attr,omitempty"`
	AdID          string         `xml:"adId,attr,omitempty"`
	UniversalAdID *UniversalAdID `xml:"UniversalAdId,omitempty"`
	Linear        *Linear        `xml:"Linear,omitempty"`
	CompanionAds  *CompanionAds  `xml:"CompanionAds,omitempty"`
	NonLinearAds  *NonLinearAds  `xml:"NonLinearAds,omitempty"`
}

// UniversalAdID (4.x mandatory for InLine ads): a registry + value
// uniquely identifying this creative across the supply chain, used
// for cross-DSP frequency capping and creative review databases.
type UniversalAdID struct {
	IDRegistry string `xml:"idRegistry,attr"`
	Value      string `xml:",chardata"`
}

// Linear is a pre/mid/post-roll video or audio ad — the player blocks
// content playback until it finishes (or the user skips, if allowed).
//
// Field order matters: the IMA SDK parser (and most other strict
// VAST 4.x clients) follow the IAB XSD child sequence:
// Duration → AdParameters → Icons → TrackingEvents → VideoClicks → MediaFiles.
// MediaFiles is the LAST child, not the second. Putting it earlier
// (the VAST 2/3 order, which is still common in the wild) trips
// VAST_LOAD_TIMEOUT / inner=6 in IMA even though xmllint considers
// the XML well-formed.
type Linear struct {
	SkipOffset     string          `xml:"skipoffset,attr,omitempty"` // "HH:MM:SS" or "N%"
	Duration       Duration        `xml:"Duration"`
	AdParameters   string          `xml:"AdParameters,omitempty"`
	TrackingEvents *TrackingEvents `xml:"TrackingEvents,omitempty"`
	VideoClicks    *VideoClicks    `xml:"VideoClicks,omitempty"`
	MediaFiles     MediaFiles      `xml:"MediaFiles"`
}

// TrackingEvents holds the list of (event, URL) beacons the player
// fires as playback progresses. Standard events: start, firstQuartile,
// midpoint, thirdQuartile, complete, mute, unmute, pause, resume, skip,
// fullscreen, exitFullscreen, progress (with offset attr).
type TrackingEvents struct {
	Tracking []Tracking `xml:"Tracking"`
}

// Tracking is a single event → URL pair. Offset is set on event=
// "progress" trackings (e.g. offset="00:00:05" to fire 5s in).
type Tracking struct {
	Event  string `xml:"event,attr"`
	Offset string `xml:"offset,attr,omitempty"`
	URI    string `xml:",cdata"`
}

// VideoClicks groups click-through (where the user lands) and
// click-tracking (additional pixels to fire on click) URLs. CustomClick
// is for player-extension hooks (rarely used).
type VideoClicks struct {
	ClickThrough  *ClickURL  `xml:"ClickThrough,omitempty"`
	ClickTracking []ClickURL `xml:"ClickTracking,omitempty"`
	CustomClick   []ClickURL `xml:"CustomClick,omitempty"`
}

// ClickURL pairs an optional ID with the URL. ID is opaque (passed
// through so the trafficker can see which click URL fired).
type ClickURL struct {
	ID  string `xml:"id,attr,omitempty"`
	URI string `xml:",cdata"`
}

// MediaFiles wraps the list of available media file variants. Players
// pick the best fit for the current viewport / bandwidth / supported
// codecs.
type MediaFiles struct {
	MediaFiles    []MediaFile    `xml:"MediaFile"`
	Mezzanine     *Mezzanine     `xml:"Mezzanine,omitempty"`
	InteractiveCreative *InteractiveCreative `xml:"InteractiveCreativeFile,omitempty"`
}

// MediaFile is one downloadable / streamable encoding of the creative.
// For audio creatives, Width / Height are left at 0 — the marshaller
// omits them via the omitempty tag.
type MediaFile struct {
	Delivery     string `xml:"delivery,attr"`     // "progressive" or "streaming"
	Type         string `xml:"type,attr"`         // MIME type, e.g. "video/mp4" / "audio/mpeg"
	Bitrate      int    `xml:"bitrate,attr,omitempty"`
	MinBitrate   int    `xml:"minBitrate,attr,omitempty"`
	MaxBitrate   int    `xml:"maxBitrate,attr,omitempty"`
	Width        int    `xml:"width,attr,omitempty"`
	Height       int    `xml:"height,attr,omitempty"`
	Codec        string `xml:"codec,attr,omitempty"`
	Scalable     string `xml:"scalable,attr,omitempty"`
	MaintainAR   string `xml:"maintainAspectRatio,attr,omitempty"`
	URI          string `xml:",cdata"`
}

// Mezzanine is the high-quality master file used by SSAI servers as
// the source for transcoding. Not played directly.
type Mezzanine struct {
	Delivery string `xml:"delivery,attr"`
	Type     string `xml:"type,attr"`
	Width    int    `xml:"width,attr,omitempty"`
	Height   int    `xml:"height,attr,omitempty"`
	URI      string `xml:",cdata"`
}

// InteractiveCreative carries a VPAID / OMID / SIMID file for
// interactive creatives. APIFramework is "VPAID" / "OMID" / "SIMID".
type InteractiveCreative struct {
	Type         string `xml:"type,attr"`
	APIFramework string `xml:"apiFramework,attr"`
	URI          string `xml:",cdata"`
}

// CompanionAds groups the companion banner(s) shown alongside the
// linear creative. Companions render in adjacent slots and fire their
// own tracking when displayed.
type CompanionAds struct {
	Required   string      `xml:"required,attr,omitempty"` // "all" | "any" | "none"
	Companions []Companion `xml:"Companion"`
}

// Companion is one companion banner — a static image, HTML snippet,
// or iframe referenced alongside the linear ad.
type Companion struct {
	ID                     string          `xml:"id,attr,omitempty"`
	Width                  int             `xml:"width,attr"`
	Height                 int             `xml:"height,attr"`
	StaticResource         *StaticResource `xml:"StaticResource,omitempty"`
	HTMLResource           *HTMLResource   `xml:"HTMLResource,omitempty"`
	IFrameResource         string          `xml:"IFrameResource,omitempty"`
	TrackingEvents         *TrackingEvents `xml:"TrackingEvents,omitempty"`
	CompanionClickThrough  string          `xml:"CompanionClickThrough,omitempty"`
	CompanionClickTracking []ClickURL      `xml:"CompanionClickTracking,omitempty"`
}

// StaticResource references an image companion.
type StaticResource struct {
	CreativeType string `xml:"creativeType,attr"` // MIME type
	URI          string `xml:",cdata"`
}

// HTMLResource is an inline HTML companion.
type HTMLResource struct {
	XMLEncoded string `xml:"xmlEncoded,attr,omitempty"`
	Content    string `xml:",cdata"`
}

// NonLinearAds describes overlay creatives — text or image strips that
// sit on top of the video without blocking playback. Not generated
// today but the element exists so consumers don't fail on unknown
// elements when proxying third-party VAST.
type NonLinearAds struct {
	TrackingEvents *TrackingEvents `xml:"TrackingEvents,omitempty"`
	NonLinears     []NonLinear     `xml:"NonLinear"`
}

// NonLinear is one overlay variant.
type NonLinear struct {
	ID                    string          `xml:"id,attr,omitempty"`
	Width                 int             `xml:"width,attr"`
	Height                int             `xml:"height,attr"`
	MinSuggestedDuration  string          `xml:"minSuggestedDuration,attr,omitempty"`
	StaticResource        *StaticResource `xml:"StaticResource,omitempty"`
	NonLinearClickThrough string          `xml:"NonLinearClickThrough,omitempty"`
}

// Duration is a VAST duration value formatted as "HH:MM:SS" or
// "HH:MM:SS.mmm". The MarshalText below handles the conversion from
// time.Duration so callers pass a typed value.
type Duration time.Duration

// UnmarshalText parses the VAST duration string produced by
// MarshalText (HH:MM:SS or HH:MM:SS.mmm) back into a Duration. Needed
// so the encoding/xml round-trip works in tests + so consumers that
// parse incoming VAST documents (Wrapper redirects, third-party VAST
// proxied through our exchange) get a usable typed value.
func (d *Duration) UnmarshalText(text []byte) error {
	s := string(text)
	if s == "" {
		*d = 0
		return nil
	}
	var h, m, sec int
	var ms int
	// HH:MM:SS or HH:MM:SS.mmm
	parts := strings.SplitN(s, ".", 2)
	hms := parts[0]
	hmsParts := strings.Split(hms, ":")
	if len(hmsParts) != 3 {
		return fmt.Errorf("vast: bad duration %q (want HH:MM:SS[.mmm])", s)
	}
	if _, err := fmt.Sscanf(hms, "%d:%d:%d", &h, &m, &sec); err != nil {
		return fmt.Errorf("vast: bad duration %q: %w", s, err)
	}
	if len(parts) == 2 {
		if _, err := fmt.Sscanf(parts[1], "%d", &ms); err != nil {
			return fmt.Errorf("vast: bad duration ms %q: %w", s, err)
		}
	}
	*d = Duration(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(sec)*time.Second + time.Duration(ms)*time.Millisecond)
	return nil
}

// MarshalText implements encoding.TextMarshaler so the marshaller
// emits a VAST-shaped duration string in the chardata position.
func (d Duration) MarshalText() ([]byte, error) {
	td := time.Duration(d)
	if td < 0 {
		td = 0
	}
	h := int(td / time.Hour)
	td -= time.Duration(h) * time.Hour
	m := int(td / time.Minute)
	td -= time.Duration(m) * time.Minute
	s := int(td / time.Second)
	td -= time.Duration(s) * time.Second
	ms := int(td / time.Millisecond)
	if ms == 0 {
		return []byte(fmt.Sprintf("%02d:%02d:%02d", h, m, s)), nil
	}
	return []byte(fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, ms)), nil
}

// URL-bearing string fields elsewhere in this package use the
// `xml:",cdata"` tag directly so they emit as <![CDATA[ url ]]>.
// VAST traditionally CDATA-wraps every URL so XML-special characters
// in query strings (&, <, >) don't need escaping. Players also tolerate
// unescaped chardata but CDATA is what every reference generator
// produces, so we match.
