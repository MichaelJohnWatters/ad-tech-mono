package vmap

import (
	"bytes"
	"encoding/xml"
	"strings"
)

// BreakSpec is the simplified input the rest of the platform constructs
// to produce one AdBreak in a VMAP schedule. Mirrors what the publisher
// arbitration code already knows about a break: when it fires, what
// role it plays, what VAST sits in it, and the tracking URLs to fire
// on break-level events.
//
// Exactly one of InlineVAST / AdTagURL should be populated. InlineVAST
// is the dynamic-served path (our exchange has already run the auction
// and produced a VAST document); AdTagURL is the deferred-fetch path
// (the player will request the VAST from us at break time, useful when
// per-user targeting needs the actual play context).
type BreakSpec struct {
	// BreakID is an opaque identifier the publisher uses to attribute
	// fills downstream. Pass through to AdBreak.breakId.
	BreakID string
	// Offset says when in the content the break fires. Default (zero
	// value) is "start" — convenient for pre-roll demos.
	Offset TimeOffset
	// BreakType is the IAB break kind. Defaults to "linear".
	BreakType string
	// InlineVAST: a marshalled vast.VAST document already built via
	// pkg/vast. The builder marshals it inside <vmap:VASTAdData>.
	InlineVAST *VAST_Inline
	// AdTagURL: deferred-fetch URL alternative to InlineVAST.
	AdTagURL string
	// AdTagTemplate is the AdTagURI templateType attribute when using
	// AdTagURL ("vast2" / "vast3" / "vast4" / "vast4.2"). Defaults to
	// "vast4.2".
	AdTagTemplate string
	// AllowMultipleAds enables ad pods inside the break.
	AllowMultipleAds bool
	// FollowRedirects controls whether the player chases VAST Wrapper
	// redirects under this AdSource. Default true.
	FollowRedirects *bool
	// Trackers is the per-break tracking URL set (breakStart/breakEnd/
	// error). Each list can have zero or more URLs.
	Trackers BreakTrackers
}

// VAST_Inline carries the bytes of a marshalled VAST document. We take
// raw bytes so callers can inline output from pkg/vast.BuildLinearAd /
// BuildPod without re-parsing.
//
// Underscore in the name keeps it from colliding with vast.VAST when
// both packages are imported. Conventionally would just be Inline but
// avoiding the shadow is worth the typo.
type VAST_Inline struct {
	XML []byte
}

// BreakTrackers groups the break-level event URL lists. Each list can
// have zero or more entries; empty lists are omitted from the output.
type BreakTrackers struct {
	BreakStart []string
	BreakEnd   []string
	Error      []string
}

// BuildSchedule produces a VMAP 1.0 document from a list of break
// specs. Returns the marshalled XML ready to serve as the response to
// a player's VMAP request. The XML declaration is included on the
// first line so the document can be served directly without further
// wrapping.
func BuildSchedule(specs []BreakSpec) ([]byte, error) {
	doc := VMAP{
		XMLNS:   "http://www.iab.net/videosuite/vmap",
		Version: Version,
	}
	for _, s := range specs {
		ab, err := specToBreak(s)
		if err != nil {
			return nil, err
		}
		doc.AdBreaks = append(doc.AdBreaks, ab)
	}
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	enc := xml.NewEncoder(&buf)
	enc.Indent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func specToBreak(s BreakSpec) (AdBreak, error) {
	bt := s.BreakType
	if bt == "" {
		bt = "linear"
	}
	follow := true
	if s.FollowRedirects != nil {
		follow = *s.FollowRedirects
	}
	src := &AdSource{
		ID:               s.BreakID,
		AllowMultipleAds: s.AllowMultipleAds,
		FollowRedirects:  follow,
	}
	switch {
	case s.InlineVAST != nil && len(s.InlineVAST.XML) > 0:
		src.VASTAdData = &VASTAdData{Inner: stripXMLDecl(s.InlineVAST.XML)}
	case s.AdTagURL != "":
		tmpl := s.AdTagTemplate
		if tmpl == "" {
			tmpl = "vast4.2"
		}
		src.AdTagURI = &AdTagURI{TemplateType: tmpl, URI: s.AdTagURL}
	}
	br := AdBreak{
		BreakType:  bt,
		TimeOffset: s.Offset.Format(),
		BreakID:    s.BreakID,
		AdSource:   src,
	}
	if te := buildBreakTracking(s.Trackers); te != nil {
		br.Tracking = te
	}
	return br, nil
}

func buildBreakTracking(t BreakTrackers) *TrackingEvents {
	var entries []Tracking
	add := func(event string, urls []string) {
		for _, u := range urls {
			entries = append(entries, Tracking{Event: event, URI: u})
		}
	}
	add("breakStart", t.BreakStart)
	add("breakEnd", t.BreakEnd)
	add("error", t.Error)
	if len(entries) == 0 {
		return nil
	}
	return &TrackingEvents{Tracking: entries}
}

// stripXMLDecl removes a leading <?xml ... ?> processing instruction
// from a marshalled XML document. encoding/xml emits one by default
// (via xml.Header), but the result of BuildLinearAd / BuildPod is
// embedded inside another XML document here, where a nested PI is
// invalid per the XML spec — strict VMAP parsers reject the whole
// document. Trimming surrounding whitespace too so the inline content
// nests cleanly under the VASTAdData element's indented start tag.
func stripXMLDecl(in []byte) string {
	s := strings.TrimLeft(string(in), " \t\r\n")
	if strings.HasPrefix(s, "<?xml") {
		if end := strings.Index(s, "?>"); end > 0 {
			s = s[end+2:]
		}
	}
	return strings.TrimSpace(s)
}

// PreRoll is sugar for the most common case: a single pre-roll break
// with an inline VAST + standard break tracking. Returns a BreakSpec
// the caller can pass straight into BuildSchedule.
func PreRoll(breakID string, vastXML []byte, trackers BreakTrackers) BreakSpec {
	return BreakSpec{
		BreakID:    breakID,
		Offset:     TimeOffset{Start: true},
		InlineVAST: &VAST_Inline{XML: vastXML},
		Trackers:   trackers,
	}
}

// MidRoll is sugar for a mid-roll break at the given offset.
func MidRoll(breakID string, at TimeOffset, vastXML []byte, trackers BreakTrackers) BreakSpec {
	return BreakSpec{
		BreakID:    breakID,
		Offset:     at,
		InlineVAST: &VAST_Inline{XML: vastXML},
		Trackers:   trackers,
	}
}

// PostRoll is sugar for a post-roll break.
func PostRoll(breakID string, vastXML []byte, trackers BreakTrackers) BreakSpec {
	return BreakSpec{
		BreakID:    breakID,
		Offset:     TimeOffset{End: true},
		InlineVAST: &VAST_Inline{XML: vastXML},
		Trackers:   trackers,
	}
}
