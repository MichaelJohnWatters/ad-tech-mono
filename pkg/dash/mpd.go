// Package dash models the MPEG-DASH Media Presentation Description (MPD) needed
// for server-side ad insertion. DASH SSAI uses the multi-period model: content
// is one or more Periods, and each ad break becomes an ad Period spliced between
// content Periods. Ad Periods reference the SAME conditioned CMAF segments the
// HLS path uses (that's the point of CMAF — one set of segments, two manifests).
//
// This file is the pure model + XML (de)serialisation, no I/O. The stitcher
// (cmd/ssai) assembles multi-period MPDs from a content MPD + conditioned ads.
package dash

import (
	"encoding/xml"
	"fmt"
	"strings"
)

// MPD is the root Media Presentation Description.
type MPD struct {
	XMLName                   xml.Name `xml:"MPD"`
	Xmlns                     string   `xml:"xmlns,attr"`
	Profiles                  string   `xml:"profiles,attr"`
	Type                      string   `xml:"type,attr"`                                // "static" (VOD) | "dynamic" (live)
	MediaPresentationDuration string   `xml:"mediaPresentationDuration,attr,omitempty"` // ISO-8601, e.g. PT18S
	MinBufferTime             string   `xml:"minBufferTime,attr"`
	Periods                   []Period `xml:"Period"`
}

// Period is a contiguous span of the presentation. In SSAI each content chunk
// and each ad is its own Period, played back-to-back.
type Period struct {
	ID             string          `xml:"id,attr"`
	Duration       string          `xml:"duration,attr,omitempty"` // ISO-8601
	EventStreams   []EventStream   `xml:"EventStream"`
	AdaptationSets []AdaptationSet `xml:"AdaptationSet"`
}

// EventStream is MPD-level timed metadata: dash.js fires each Event at its
// presentationTime during playback, so a client can attribute ad quartiles at
// the moment they actually play (unlike the server beacons, which fire on
// segment fetch). SSAI uses it to surface the ad's quartile timeline.
type EventStream struct {
	SchemeIDURI string  `xml:"schemeIdUri,attr"`
	Value       string  `xml:"value,attr,omitempty"`
	Timescale   int     `xml:"timescale,attr"`
	Events      []Event `xml:"Event"`
}

// Event is one timed-metadata point within an EventStream (period-relative time).
type Event struct {
	PresentationTime int    `xml:"presentationTime,attr"`
	Duration         int    `xml:"duration,attr,omitempty"`
	ID               string `xml:"id,attr,omitempty"`
	Body             string `xml:",chardata"` // event name (e.g. "firstQuartile")
}

// QuartileScheme is the schemeIdUri the player subscribes to for ad quartile
// events emitted by the SSAI stitcher.
const QuartileScheme = "urn:adtech:ssai:quartile"

// AdaptationSet groups interchangeable Representations (e.g. all video rungs).
type AdaptationSet struct {
	MimeType         string           `xml:"mimeType,attr"`
	ContentType      string           `xml:"contentType,attr,omitempty"`
	SegmentAlignment bool             `xml:"segmentAlignment,attr,omitempty"`
	Lang             string           `xml:"lang,attr,omitempty"`
	Representations  []Representation `xml:"Representation"`
}

// Representation is one encoding (rung) — a bandwidth/resolution + its segments.
type Representation struct {
	ID          string       `xml:"id,attr"`
	Bandwidth   int          `xml:"bandwidth,attr"`
	Codecs      string       `xml:"codecs,attr,omitempty"`
	Width       int          `xml:"width,attr,omitempty"`
	Height      int          `xml:"height,attr,omitempty"`
	AudioRate   int          `xml:"audioSamplingRate,attr,omitempty"`
	SegmentList *SegmentList `xml:"SegmentList"`
}

// SegmentList enumerates a representation's segments explicitly (init + media).
// SSAI uses SegmentList — not SegmentTemplate — because spliced ad/content
// segments don't share a numbering template.
type SegmentList struct {
	Timescale      int             `xml:"timescale,attr"`
	Duration       int             `xml:"duration,attr,omitempty"`
	Initialization *Initialization `xml:"Initialization"`
	SegmentURLs    []SegmentURL    `xml:"SegmentURL"`
}

// Initialization is the fMP4/CMAF init segment (the #EXT-X-MAP equivalent).
type Initialization struct {
	SourceURL string `xml:"sourceURL,attr"`
}

// SegmentURL is one media segment.
type SegmentURL struct {
	Media string `xml:"media,attr"`
}

const (
	xmlnsDASH   = "urn:mpeg:dash:schema:mpd:2011"
	profileMain = "urn:mpeg:dash:profile:isoff-main:2011"
)

// NewVOD returns an empty static (VOD) MPD ready to append Periods to.
func NewVOD() *MPD {
	return &MPD{
		Xmlns:         xmlnsDASH,
		Profiles:      profileMain,
		Type:          "static",
		MinBufferTime: "PT2S",
	}
}

// XML serialises the MPD with the XML declaration prepended.
func (m *MPD) XML() (string, error) {
	body, err := xml.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	return xml.Header + string(body) + "\n", nil
}

// ParseMPD deserialises an MPD document.
func ParseMPD(data string) (*MPD, error) {
	var m MPD
	if err := xml.Unmarshal([]byte(data), &m); err != nil {
		return nil, fmt.Errorf("dash: parse MPD: %w", err)
	}
	if len(m.Periods) == 0 {
		return nil, fmt.Errorf("dash: MPD has no periods")
	}
	return &m, nil
}

// IsMPD reports whether a document looks like a DASH MPD (vs HLS/other).
func IsMPD(data string) bool {
	return strings.Contains(data, "<MPD") && strings.Contains(data, "dash:schema:mpd")
}

// Duration returns an ISO-8601 duration for a number of seconds (e.g. 18 → PT18S,
// 6.5 → PT6.5S). Used for Period/MPD durations.
func Duration(seconds float64) string {
	if seconds == float64(int64(seconds)) {
		return fmt.Sprintf("PT%dS", int64(seconds))
	}
	return fmt.Sprintf("PT%sS", trimFloat(seconds))
}

func trimFloat(f float64) string {
	s := fmt.Sprintf("%.3f", f)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	return s
}
