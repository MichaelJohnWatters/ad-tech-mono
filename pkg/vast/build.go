package vast

import (
	"bytes"
	"encoding/xml"
	"strings"
	"time"
)

// LinearSpec is the simplified input the rest of the platform
// constructs to produce a VAST document. Mirrors the fields the DSP /
// ad server already track for a video creative + the tracker URLs the
// existing macros package generates for display creatives, so wiring
// the video path through doesn't introduce a new domain concept.
//
// MediaFiles can contain video/* or audio/* MIMEs (or both — players
// will pick the right kind for the slot). Setting Width / Height on
// an audio MediaFile is allowed but conventionally left at 0.
type LinearSpec struct {
	AdID       string        // platform ad ID (DSP's bid.id or creative.id)
	AdSystem   string        // "adtech-mono" by convention
	AdTitle    string        // human label shown in debug overlays
	Advertiser string        // advertiser domain
	Duration   time.Duration // creative play time
	MediaFiles []MediaFile   // one or more encoded variants
	Trackers   LinearTrackers
	Click      ClickSpec
	Sequence   int      // pod position (0 = standalone)
	Pricing    *Pricing // optional; omitted from the XML when nil
	// UniversalAdID is required by VAST 4.x for InLine ads. Registry +
	// value uniquely identify the creative across the supply chain.
	// Defaults to ("ad-tech-mono", AdID) when zero.
	UniversalAdID UniversalAdID
}

// LinearTrackers groups the per-event tracking URL lists. Each list can
// have zero or more URLs; lists with no URLs are omitted from the XML.
// Field set matches what the existing pkg/adserving/macros.go already
// produces for display creatives — same naming so wiring sites are
// obvious.
type LinearTrackers struct {
	Impression    []string
	Start         []string
	FirstQuartile []string
	Midpoint      []string
	ThirdQuartile []string
	Complete      []string
	Mute          []string
	Unmute        []string
	Pause         []string
	Resume        []string
	Skip          []string
	Fullscreen    []string
	// Progress is a list of (offset, URL) pairs that fire at the given
	// playback offsets. Offset is the player position when the URL
	// should fire — "00:00:05" to fire 5s in, etc.
	Progress []ProgressTracker
}

// ProgressTracker is one (offset, URL) entry for an event="progress"
// tracking element.
type ProgressTracker struct {
	Offset time.Duration
	URL    string
}

// ClickSpec is the click-through + click-tracking pair. ClickThrough
// is where the user lands; ClickTracking is the pixel(s) we also fire.
// Mirrors the redir + click-tracker URL the display side already
// generates.
type ClickSpec struct {
	ClickThrough  string
	ClickTracking []string
}

// BuildLinearAd produces a VAST 4.2 document for a linear ad and
// returns the marshalled XML. Caller-friendly: pass a populated
// LinearSpec, get back ready-to-serve bytes.
//
// The output is pretty-printed with two-space indents — the few extra
// bytes versus a minified document are worth it for human inspection
// inside trace explorers / Jaeger spans. Real players don't care
// about whitespace either way.
func BuildLinearAd(spec LinearSpec) ([]byte, error) {
	doc := VAST{
		Version: Version,
		Ads:     []Ad{specToAd(spec)},
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

// BuildPod produces a VAST document with one Ad per spec, sequenced
// in order — the standard shape for pre/mid/post-roll pods. Players
// iterate ads by Sequence and play them back-to-back.
func BuildPod(specs []LinearSpec) ([]byte, error) {
	doc := VAST{Version: Version, Ads: make([]Ad, 0, len(specs))}
	for i, s := range specs {
		if s.Sequence == 0 {
			s.Sequence = i + 1
		}
		doc.Ads = append(doc.Ads, specToAd(s))
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

// IsAudioOnly reports whether every MediaFile in the spec is an audio
// MIME. Used by the builder to skip emitting width/height on Linear
// creatives that have no visual component (a player rendering an
// audio-only VAST wouldn't show a slot for the missing dimensions
// anyway, but skipping keeps the document spec-compliant).
func IsAudioOnly(spec LinearSpec) bool {
	if len(spec.MediaFiles) == 0 {
		return false
	}
	for _, mf := range spec.MediaFiles {
		if !strings.HasPrefix(mf.Type, "audio/") {
			return false
		}
	}
	return true
}

func specToAd(spec LinearSpec) Ad {
	uad := spec.UniversalAdID
	if uad.IDRegistry == "" {
		uad = UniversalAdID{IDRegistry: "ad-tech-mono", Value: spec.AdID}
	}
	adSys := spec.AdSystem
	if adSys == "" {
		adSys = "ad-tech-mono"
	}
	imps := make([]Impression, 0, len(spec.Trackers.Impression))
	for _, u := range spec.Trackers.Impression {
		imps = append(imps, Impression{URI: u})
	}
	linear := &Linear{
		Duration: Duration(spec.Duration),
		MediaFiles: MediaFiles{
			MediaFiles: spec.MediaFiles,
		},
	}
	if te := buildTrackingEvents(spec.Trackers); te != nil {
		linear.TrackingEvents = te
	}
	if vc := buildVideoClicks(spec.Click); vc != nil {
		linear.VideoClicks = vc
	}
	return Ad{
		ID:       spec.AdID,
		Sequence: spec.Sequence,
		InLine: &InLine{
			AdSystem:    AdSystem{Name: adSys, Version: "1.0"},
			AdTitle:     spec.AdTitle,
			Advertiser:  spec.Advertiser,
			Pricing:     spec.Pricing,
			Impressions: imps,
			Creatives: Creatives{
				Creatives: []Creative{{
					ID:            spec.AdID,
					Sequence:      spec.Sequence,
					UniversalAdID: &uad,
					Linear:        linear,
				}},
			},
		},
	}
}

func buildTrackingEvents(t LinearTrackers) *TrackingEvents {
	var entries []Tracking
	add := func(event string, urls []string) {
		for _, u := range urls {
			entries = append(entries, Tracking{Event: event, URI: u})
		}
	}
	add("start", t.Start)
	add("firstQuartile", t.FirstQuartile)
	add("midpoint", t.Midpoint)
	add("thirdQuartile", t.ThirdQuartile)
	add("complete", t.Complete)
	add("mute", t.Mute)
	add("unmute", t.Unmute)
	add("pause", t.Pause)
	add("resume", t.Resume)
	add("skip", t.Skip)
	add("fullscreen", t.Fullscreen)
	for _, p := range t.Progress {
		entries = append(entries, Tracking{
			Event:  "progress",
			Offset: durationToVAST(p.Offset),
			URI:    p.URL,
		})
	}
	if len(entries) == 0 {
		return nil
	}
	return &TrackingEvents{Tracking: entries}
}

func buildVideoClicks(c ClickSpec) *VideoClicks {
	if c.ClickThrough == "" && len(c.ClickTracking) == 0 {
		return nil
	}
	vc := &VideoClicks{}
	if c.ClickThrough != "" {
		vc.ClickThrough = &ClickURL{URI: c.ClickThrough}
	}
	for _, u := range c.ClickTracking {
		vc.ClickTracking = append(vc.ClickTracking, ClickURL{URI: u})
	}
	return vc
}

// durationToVAST formats a time.Duration as the HH:MM:SS string VAST
// expects for offset attributes. Shared between Duration.MarshalText
// (above) and tracking offsets so the formats stay aligned.
func durationToVAST(d time.Duration) string {
	b, _ := Duration(d).MarshalText()
	return string(b)
}
