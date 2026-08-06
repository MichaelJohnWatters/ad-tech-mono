// Package vmap generates VMAP 1.0 XML — the IAB-standard schedule
// document video players fetch once at content start to know when to
// pause for ad breaks and what to play in each break.
//
// Conceptually:
//   - Player loads the video → requests a VMAP from the publisher.
//   - VMAP carries 0..N AdBreak elements, each with a timeOffset
//     ("start", "00:05:00", "25%", "end") and an AdSource that points at
//     a VAST document either inline (VASTAdData) or by URL (AdTagURI).
//   - Player honours the timeOffsets to pause content and run the
//     referenced VAST in the slot.
//
// Spec reference: https://iabtechlab.com/wp-content/uploads/2018/06/VMAP_1.0.1.pdf
//
// We pair this with pkg/vast: VMAP says when, VAST says what. Together
// they cover the dynamic-ad-insertion / client-side ad scheduling story
// for instream video. SSAI (server-side stitching, Phase 9 step 87)
// also uses VMAP — the SSAI manager consults it to know which content
// segments to splice ads into before serving the manifest.
package vmap

import (
	"encoding/xml"
	"fmt"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/vast"
)

// Version is the VMAP version string emitted in the root element.
// 1.0.1 is the current stable; players treat 1.0 and 1.0.1 the same.
const Version = "1.0"

// VMAP is the root element of a VMAP document. The xmlns attribute is
// part of the spec and players check for it (or treat the document as
// invalid VAST) — we always emit it.
type VMAP struct {
	XMLName  xml.Name  `xml:"vmap:VMAP"`
	XMLNS    string    `xml:"xmlns:vmap,attr"`
	Version  string    `xml:"version,attr"`
	AdBreaks []AdBreak `xml:"vmap:AdBreak"`
}

// AdBreak is one slot in the schedule. TimeOffset is a string because
// VMAP allows multiple formats: "start", "end", "HH:MM:SS[.mmm]" for
// absolute offsets, or "N%" for relative offsets.
type AdBreak struct {
	BreakType   string          `xml:"breakType,attr"`             // linear, nonlinear, display (linear is the common case)
	TimeOffset  string          `xml:"timeOffset,attr"`            // start | end | HH:MM:SS | N%
	BreakID     string          `xml:"breakId,attr,omitempty"`     // opaque publisher-side identifier
	RepeatAfter string          `xml:"repeatAfter,attr,omitempty"` // HH:MM:SS — auto-repeat the break at this cadence
	AdSource    *AdSource       `xml:"vmap:AdSource,omitempty"`
	Tracking    *TrackingEvents `xml:"vmap:TrackingEvents,omitempty"`
	Extensions  *Extensions     `xml:"vmap:Extensions,omitempty"`
}

// AdSource carries the actual VAST that plays in the break. Exactly
// one of VASTAdData / AdTagURI / CustomAdData should be populated;
// players that find multiple use VASTAdData first.
type AdSource struct {
	ID               string      `xml:"id,attr,omitempty"`
	AllowMultipleAds bool        `xml:"allowMultipleAds,attr"`       // true = pods allowed in this break
	FollowRedirects  bool        `xml:"followRedirects,attr"`        // controls Wrapper chasing
	VASTAdData       *VASTAdData `xml:"vmap:VASTAdData,omitempty"`   // inline VAST document
	AdTagURI         *AdTagURI   `xml:"vmap:AdTagURI,omitempty"`     // URL to fetch VAST from
	CustomAdData     *CustomData `xml:"vmap:CustomAdData,omitempty"` // non-VAST source (rare)
}

// VASTAdData wraps an inline VAST document so it can be embedded in the
// VMAP. The Inner field is the marshalled VAST XML and gets injected as
// raw chardata — VMAP players parse it back through their VAST parser.
type VASTAdData struct {
	Inner string `xml:",innerxml"`
}

// AdTagURI points at an external VAST document. TemplateType tells the
// player which parser to use ("vast2", "vast3", "vast4", "vast4.1",
// "vast4.2"). Players that don't recognise the template type fall back
// to VAST 4.x parsing.
type AdTagURI struct {
	TemplateType string `xml:"templateType,attr,omitempty"`
	URI          string `xml:",cdata"`
}

// CustomData carries a non-VAST ad source (proprietary formats like
// SCTE-35 in-band cues for SSAI). TemplateType identifies the format.
type CustomData struct {
	TemplateType string `xml:"templateType,attr,omitempty"`
	Content      string `xml:",innerxml"`
}

// TrackingEvents is the per-break tracking list. Distinct from VAST's
// in-creative tracking — these fire on break-level events (start of
// break, end of break, error filling the break) regardless of which
// individual ad inside the pod is playing.
type TrackingEvents struct {
	Tracking []Tracking `xml:"vmap:Tracking"`
}

// Tracking pairs a break-level event with a URL the player pings when
// the event fires. Defined events: breakStart, breakEnd, error.
type Tracking struct {
	Event string `xml:"event,attr"`
	URI   string `xml:",cdata"`
}

// Extensions is the VMAP escape hatch for vendor-specific metadata.
// Each Extension has a free-form type attribute and arbitrary inner XML.
// Used in real deployments for things like sticky-ad parameters,
// publisher-side audit hooks, or SSAI session keys.
type Extensions struct {
	Extension []Extension `xml:"vmap:Extension"`
}

// Extension is one vendor-specific entry. Type identifies the schema;
// Content is opaque XML the player passes through unchanged.
type Extension struct {
	Type    string `xml:"type,attr"`
	Content string `xml:",innerxml"`
}

// TimeOffset is a typed helper for callers that prefer a Go value over
// the raw VMAP string. Convert to a string via Format() before placing
// it on AdBreak.TimeOffset.
type TimeOffset struct {
	// One of: Start=true (pre-roll), End=true (post-roll),
	// AbsoluteAt set (mid-roll at duration), PercentOf set (mid-roll
	// at percentage of content). The first non-zero field wins.
	Start      bool
	End        bool
	AbsoluteAt time.Duration
	PercentOf  int // 0–100
}

// Format renders the TimeOffset as the VMAP-shaped string. Empty input
// (no field set) is treated as "start" — the most permissive default.
func (o TimeOffset) Format() string {
	switch {
	case o.End:
		return "end"
	case o.PercentOf > 0:
		return fmt.Sprintf("%d%%", o.PercentOf)
	case o.AbsoluteAt > 0:
		// Render as HH:MM:SS[.mmm], same shape pkg/vast.Duration uses.
		td := o.AbsoluteAt
		h := int(td / time.Hour)
		td -= time.Duration(h) * time.Hour
		m := int(td / time.Minute)
		td -= time.Duration(m) * time.Minute
		s := int(td / time.Second)
		td -= time.Duration(s) * time.Second
		ms := int(td / time.Millisecond)
		if ms == 0 {
			return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
		}
		return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, ms)
	default:
		return "start"
	}
}

// EmbedVAST marshals a vast.VAST document into a VASTAdData ready to
// place inside an AdSource.VASTAdData field. Convenience wrapper so
// callers don't have to drive encoding/xml themselves and don't risk
// emitting a malformed inline VAST.
func EmbedVAST(v vast.VAST) (*VASTAdData, error) {
	// Re-emit with a small indent so the inline VAST is readable when
	// the whole VMAP gets pretty-printed downstream. The marshaller
	// inlines it under <vmap:VASTAdData> via innerxml.
	out, err := xml.MarshalIndent(v, "      ", "  ")
	if err != nil {
		return nil, err
	}
	return &VASTAdData{Inner: strings.TrimSpace(string(out))}, nil
}
