// Package ssai implements server-side ad insertion: rewriting an HLS media
// playlist so ad segments are stitched directly into the content stream. The
// player requests segments in order and can't tell content from ads — which is
// what makes SSAI unblockable, unlike client-side VAST.
//
// This file is the pure manifest-manipulation core (no I/O): parse an HLS media
// playlist, find the ad breaks delimited by #EXT-X-CUE-OUT / #EXT-X-CUE-IN,
// replace the content-during-break segments with ad segments, and render the
// result. cmd/ssai wires this to a real auction (winning ad per break) and a
// server-side beacon endpoint.
package ssai

import (
	"fmt"
	"strconv"
	"strings"
)

// Segment is one media segment in an HLS playlist: its #EXTINF duration, its
// URI, and any tags that precede it (discontinuity, cue markers).
type Segment struct {
	Duration      float64
	URI           string
	Discontinuity bool    // emit #EXT-X-DISCONTINUITY before this segment
	CueOut        float64 // >0 → emit #EXT-X-CUE-OUT:DURATION=<n> before this segment
	CueIn         bool    // emit #EXT-X-CUE-IN before this segment
	Ad            bool    // stitched ad segment (not original content)
	Map           string  // fMP4 init URI → emit #EXT-X-MAP:URI="<map>" before this segment
}

// Manifest is a parsed HLS media playlist.
type Manifest struct {
	Header   []string // verbatim lines before the first segment (#EXTM3U, VERSION, …)
	Segments []Segment
	EndList  bool // playlist carried #EXT-X-ENDLIST (VOD)
}

// BreakSpan is the half-open range [Start,End) of content segment indices that
// sit inside one #EXT-X-CUE-OUT … #EXT-X-CUE-IN, plus the advertised break
// duration from the CUE-OUT marker.
type BreakSpan struct {
	Start    int
	End      int
	Duration float64
}

// ParseMedia parses an HLS media playlist. It understands #EXTINF, segment
// URIs, #EXT-X-CUE-OUT[:DURATION=n], #EXT-X-CUE-IN, #EXT-X-DISCONTINUITY, and
// #EXT-X-ENDLIST; every other line before the first segment is preserved as
// header. Ad-break spans are recovered later via Breaks().
func ParseMedia(text string) (*Manifest, error) {
	m := &Manifest{}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")

	var pending Segment      // tags accumulate here until a URI closes a segment
	var haveInf bool         // saw an #EXTINF for the pending segment
	sawFirstSegment := false // header ends at the first #EXTINF

	// SCTE-35 DATERANGE breaks carry the break length in PLANNED-DURATION rather
	// than an explicit #EXT-X-CUE-IN, so we auto-close them in a post-pass: the
	// segments each DATERANGE-OUT opens are recorded here and closed once their
	// cumulative content duration reaches the planned length.
	var scteAuto []int   // segment indices that opened a DATERANGE SCTE35-OUT break
	pendingScte := false // the next segment starts a DATERANGE break

	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "#EXTINF:"):
			sawFirstSegment = true
			dur := parseInf(line)
			pending.Duration = dur
			haveInf = true
		case strings.HasPrefix(line, "#EXT-X-MAP"):
			// fMP4 init segment declaration; applies to the following media
			// segment. (Not a header line even before the first #EXTINF.)
			pending.Map = parseMapURI(line)
		case strings.HasPrefix(line, "#EXT-X-CUE-OUT"):
			pending.CueOut = parseCueOutDuration(line)
		case strings.HasPrefix(line, "#EXT-X-DATERANGE") && isScteOut(line):
			// Broadcast/SCTE-35 signalling: a DATERANGE with SCTE35-OUT opens an
			// ad break of PLANNED-DURATION seconds. Map it to our CUE-OUT model so
			// the stitcher treats it like any other break. A matching SCTE35-IN
			// DATERANGE (isScteIn) closes it early; otherwise the post-pass does.
			pending.CueOut = parseDaterangeDuration(line)
			pendingScte = true
		case strings.HasPrefix(line, "#EXT-X-DATERANGE") && isScteIn(line):
			pending.CueIn = true
		case line == "#EXT-X-CUE-IN":
			pending.CueIn = true
		case line == "#EXT-X-DISCONTINUITY":
			pending.Discontinuity = true
		case line == "#EXT-X-ENDLIST":
			m.EndList = true
		case strings.HasPrefix(line, "#"):
			if !sawFirstSegment {
				m.Header = append(m.Header, line)
			}
			// Tags between segments we don't model are dropped; the platform's
			// sample manifests only use the ones handled above.
		default:
			// A non-tag line is a segment URI, closing the pending segment.
			if !haveInf {
				// URI without a preceding #EXTINF — tolerate by defaulting.
				pending.Duration = 0
			}
			pending.URI = line
			m.Segments = append(m.Segments, pending)
			if pendingScte {
				scteAuto = append(scteAuto, len(m.Segments)-1)
				pendingScte = false
			}
			pending = Segment{}
			haveInf = false
		}
	}
	if len(m.Segments) == 0 {
		return nil, fmt.Errorf("ssai: no media segments found")
	}
	m.closeScteBreaks(scteAuto)
	return m, nil
}

// Breaks returns the content-segment spans inside each ad break. A break opens
// on the segment carrying CueOut and closes on the segment carrying CueIn (the
// CueIn segment is content again, so it is excluded from the span).
func (m *Manifest) Breaks() []BreakSpan {
	var breaks []BreakSpan
	open := -1
	var dur float64
	for i, s := range m.Segments {
		if s.CueOut > 0 && open == -1 {
			open = i
			dur = s.CueOut
		}
		if s.CueIn && open != -1 {
			breaks = append(breaks, BreakSpan{Start: open, End: i, Duration: dur})
			open = -1
		}
	}
	// An unterminated CUE-OUT (no CUE-IN before end) runs to the playlist end.
	if open != -1 {
		breaks = append(breaks, BreakSpan{Start: open, End: len(m.Segments), Duration: dur})
	}
	return breaks
}

// Stitch replaces each ad break's content segments with ad segments supplied by
// fill(i, span). fill returns the ad segments for break i (or nil to leave the
// content in place, e.g. on a no-fill auction). The first ad segment of a break
// is marked with a discontinuity, and a discontinuity is placed on the first
// content segment after the break, so players reset their decoder across the
// content↔ad boundary. Breaks are processed back-to-front so earlier indices
// stay valid as segments are spliced.
func (m *Manifest) Stitch(fill func(i int, span BreakSpan) []Segment) {
	// fMP4 content declares an init segment (#EXT-X-MAP). After an ad — which
	// carries its OWN init — the first content segment back must re-declare the
	// content init, or the player keeps decoding content against the ad's init.
	contentMap := activeContentMap(m.Segments)
	breaks := m.Breaks()
	for i := len(breaks) - 1; i >= 0; i-- {
		span := breaks[i]
		ads := fill(i, span)
		if len(ads) == 0 {
			continue // no fill: keep content (slate/passthrough)
		}
		for j := range ads {
			ads[j].Ad = true
		}
		ads[0].Discontinuity = true
		// Carry the CUE-OUT marker onto the first ad segment so downstream
		// still sees where the break began.
		ads[0].CueOut = span.Duration

		// Mark the first post-break content segment with a discontinuity + the
		// CUE-IN so the decoder resets back to content.
		post := span.End
		if post < len(m.Segments) {
			m.Segments[post].Discontinuity = true
			m.Segments[post].CueIn = true
			// Restore the content init after the ad (fMP4 only; no-op for TS).
			if contentMap != "" && m.Segments[post].Map == "" {
				m.Segments[post].Map = contentMap
			}
		}

		// Splice: [ :Start ] + ads + [ End: ]. The replaced content segments
		// (Start:End) are dropped — that's the "ad replaces content" model.
		out := make([]Segment, 0, len(m.Segments)-(span.End-span.Start)+len(ads))
		out = append(out, m.Segments[:span.Start]...)
		out = append(out, ads...)
		out = append(out, m.Segments[span.End:]...)
		m.Segments = out
	}
}

// Render serialises the manifest back to an HLS media playlist string.
func (m *Manifest) Render() string {
	var b strings.Builder
	for _, h := range m.Header {
		b.WriteString(h)
		b.WriteByte('\n')
	}
	if len(m.Header) == 0 {
		b.WriteString("#EXTM3U\n")
	}
	for _, s := range m.Segments {
		if s.Discontinuity {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		if s.Map != "" {
			fmt.Fprintf(&b, "#EXT-X-MAP:URI=%q\n", s.Map)
		}
		if s.CueOut > 0 {
			fmt.Fprintf(&b, "#EXT-X-CUE-OUT:DURATION=%s\n", trimFloat(s.CueOut))
		}
		if s.CueIn {
			b.WriteString("#EXT-X-CUE-IN\n")
		}
		fmt.Fprintf(&b, "#EXTINF:%s,\n", trimFloat(s.Duration))
		b.WriteString(s.URI)
		b.WriteByte('\n')
	}
	if m.EndList {
		b.WriteString("#EXT-X-ENDLIST\n")
	}
	return b.String()
}

// AdDuration returns the total duration of the ad segments in the manifest —
// useful for logging how much ad time was stitched.
func (m *Manifest) AdDuration() float64 {
	var total float64
	for _, s := range m.Segments {
		if s.Ad {
			total += s.Duration
		}
	}
	return total
}

// parseMapURI extracts the URI from an #EXT-X-MAP:URI="..." line.
func parseMapURI(line string) string {
	const key = `URI="`
	i := strings.Index(line, key)
	if i < 0 {
		return ""
	}
	rest := line[i+len(key):]
	if j := strings.IndexByte(rest, '"'); j >= 0 {
		return rest[:j]
	}
	return ""
}

// activeContentMap returns the first init URI declared in the playlist (the
// content's #EXT-X-MAP), or "" for TS content that has none.
func activeContentMap(segs []Segment) string {
	for _, s := range segs {
		if s.Map != "" {
			return s.Map
		}
	}
	return ""
}

func parseInf(line string) float64 {
	// #EXTINF:6.0,  → 6.0
	rest := strings.TrimPrefix(line, "#EXTINF:")
	if c := strings.IndexByte(rest, ','); c >= 0 {
		rest = rest[:c]
	}
	f, _ := strconv.ParseFloat(strings.TrimSpace(rest), 64)
	return f
}

func parseCueOutDuration(line string) float64 {
	// #EXT-X-CUE-OUT:DURATION=30  or  #EXT-X-CUE-OUT:30  or bare #EXT-X-CUE-OUT
	rest := strings.TrimPrefix(line, "#EXT-X-CUE-OUT")
	rest = strings.TrimPrefix(rest, ":")
	rest = strings.TrimPrefix(rest, "DURATION=")
	if rest == "" {
		return 30 // sensible default when the marker carries no duration
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
	if err != nil || f <= 0 {
		return 30
	}
	return f
}

// closeScteBreaks sets #EXT-X-CUE-IN on the first content segment past each
// DATERANGE SCTE35-OUT whose break already carries an explicit close, or —
// for pure SCTE-35 signalling with no SCTE35-IN — the segment where cumulative
// content duration first reaches PLANNED-DURATION. Without this a DATERANGE-only
// break would swallow all remaining segments.
func (m *Manifest) closeScteBreaks(starts []int) {
	for _, start := range starts {
		if start >= len(m.Segments) {
			continue
		}
		if hasCueInFrom(m.Segments, start+1) {
			continue // an explicit SCTE35-IN / CUE-IN already closes this break
		}
		planned := m.Segments[start].CueOut
		var acc float64
		for j := start; j < len(m.Segments); j++ {
			acc += m.Segments[j].Duration
			if acc >= planned {
				if j+1 < len(m.Segments) {
					m.Segments[j+1].CueIn = true
				}
				break
			}
		}
	}
}

// hasCueInFrom reports whether any segment at or after idx already carries a
// CUE-IN before the next CUE-OUT (i.e. this break is explicitly closed).
func hasCueInFrom(segs []Segment, idx int) bool {
	for j := idx; j < len(segs); j++ {
		if segs[j].CueOut > 0 {
			return false
		}
		if segs[j].CueIn {
			return true
		}
	}
	return false
}

func isScteOut(line string) bool {
	return strings.Contains(line, "SCTE35-OUT") || strings.Contains(strings.ToUpper(line), "CUE=\"OUT")
}

func isScteIn(line string) bool {
	return strings.Contains(line, "SCTE35-IN") || strings.Contains(strings.ToUpper(line), "CUE=\"IN")
}

// parseDaterangeDuration reads PLANNED-DURATION / DURATION (seconds) from an
// #EXT-X-DATERANGE line, defaulting to 30s when absent.
func parseDaterangeDuration(line string) float64 {
	for _, key := range []string{"PLANNED-DURATION=", "DURATION="} {
		i := strings.Index(line, key)
		if i < 0 {
			continue
		}
		rest := line[i+len(key):]
		if c := strings.IndexByte(rest, ','); c >= 0 {
			rest = rest[:c]
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(rest), 64); err == nil && f > 0 {
			return f
		}
	}
	return 30
}

func trimFloat(f float64) string {
	s := strconv.FormatFloat(f, 'f', 3, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "" {
		return "0"
	}
	return s
}

// SegmentAds splits a total ad duration into segments of segDur seconds each
// (the last segment carries the remainder), building a Segment per slice via
// uri(n, segStart). This turns a single ad media file + duration into the
// per-segment entries an HLS playlist needs.
func SegmentAds(totalDur, segDur float64, uri func(n int, start float64) string) []Segment {
	if segDur <= 0 {
		segDur = 6
	}
	var segs []Segment
	n := 0
	for start := 0.0; start < totalDur-0.001; start += segDur {
		d := segDur
		if remaining := totalDur - start; remaining < segDur {
			d = remaining
		}
		segs = append(segs, Segment{Duration: d, URI: uri(n, start)})
		n++
	}
	if len(segs) == 0 { // totalDur smaller than one segment
		segs = append(segs, Segment{Duration: totalDur, URI: uri(0, 0)})
	}
	return segs
}
