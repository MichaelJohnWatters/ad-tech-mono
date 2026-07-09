package ssai

import (
	"fmt"
	"strconv"
	"strings"
)

// This file handles the HLS MASTER playlist (the ABR ladder), distinct from the
// media playlists in manifest.go. A master lists the available bitrate rungs
// (#EXT-X-STREAM-INF + a variant playlist URI each); the player picks one and
// adapts. For SSAI the stitcher rewrites each variant URI to point back at
// itself, so every rung is stitched independently against a profile-matched ad.
//
// Demuxed masters also carry #EXT-X-MEDIA renditions — a separate audio (or
// subtitle) group that variants reference via AUDIO="grp". Those group playlists
// must be stitched too (and their URIs rewritten), or the audio wouldn't carry
// the ad; and the variants' AUDIO=/SUBTITLES= attributes must survive the
// rewrite, or the player loses its audio track.

// Variant is one rung of an ABR master playlist.
type Variant struct {
	URI       string
	Bandwidth int    // bits/sec (#EXT-X-STREAM-INF BANDWIDTH)
	Width     int    // optional RESOLUTION width
	Height    int    // optional RESOLUTION height
	Codecs    string // optional CODECS
	Attrs     string // raw #EXT-X-STREAM-INF attribute list; when set it is re-emitted verbatim (preserves AUDIO=, FRAME-RATE=, …)
}

// Media is an #EXT-X-MEDIA alternate rendition (a demuxed audio/subtitle group).
type Media struct {
	Type    string // AUDIO | SUBTITLES | CLOSED-CAPTIONS | VIDEO
	GroupID string // GROUP-ID
	URI     string // rendition playlist URI (may be empty, e.g. CLOSED-CAPTIONS)
	Attrs   string // raw attribute list, re-emitted verbatim with only URI swapped
}

// IsMaster reports whether an HLS playlist is a master (has #EXT-X-STREAM-INF)
// rather than a media playlist (has #EXTINF segments).
func IsMaster(text string) bool {
	return strings.Contains(text, "#EXT-X-STREAM-INF")
}

// ParseMaster extracts the variant rungs from a master playlist. Each
// #EXT-X-STREAM-INF attribute line is followed by the variant playlist URI. The
// raw attribute list is retained on Variant.Attrs so a rewrite preserves
// attributes we don't model (AUDIO, SUBTITLES, FRAME-RATE, …).
func ParseMaster(text string) []Variant {
	var out []Variant
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var pending *Variant
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			attrs := strings.TrimPrefix(line, "#EXT-X-STREAM-INF:")
			v := parseStreamInf(attrs)
			v.Attrs = attrs
			pending = &v
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if pending != nil {
			pending.URI = line
			out = append(out, *pending)
			pending = nil
		}
	}
	return out
}

// ParseRenditions extracts the #EXT-X-MEDIA alternate renditions from a master.
func ParseRenditions(text string) []Media {
	var out []Media
	for _, raw := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "#EXT-X-MEDIA:") {
			continue
		}
		out = append(out, parseMediaTag(strings.TrimPrefix(line, "#EXT-X-MEDIA:")))
	}
	return out
}

// BuildMaster renders a master playlist from its renditions + variants. Renditions
// (#EXT-X-MEDIA) are emitted first so variants can reference their groups.
func BuildMaster(variants []Variant, media []Media) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n")
	for _, m := range media {
		b.WriteString("#EXT-X-MEDIA:")
		b.WriteString(setAttr(m.Attrs, "URI", m.URI))
		b.WriteByte('\n')
	}
	for _, v := range variants {
		if v.Attrs != "" {
			// Re-emit the original attribute list verbatim (keeps AUDIO= etc.).
			b.WriteString("#EXT-X-STREAM-INF:")
			b.WriteString(v.Attrs)
		} else {
			b.WriteString("#EXT-X-STREAM-INF:BANDWIDTH=")
			b.WriteString(strconv.Itoa(v.Bandwidth))
			if v.Width > 0 && v.Height > 0 {
				fmt.Fprintf(&b, ",RESOLUTION=%dx%d", v.Width, v.Height)
			}
			if v.Codecs != "" {
				fmt.Fprintf(&b, ",CODECS=%q", v.Codecs)
			}
		}
		b.WriteByte('\n')
		b.WriteString(v.URI)
		b.WriteByte('\n')
	}
	return b.String()
}

// parseStreamInf pulls BANDWIDTH / RESOLUTION / CODECS out of an EXT-X-STREAM-INF
// attribute list (comma-separated, values may be quoted).
func parseStreamInf(attrs string) Variant {
	var v Variant
	for _, kv := range splitAttrs(attrs) {
		i := strings.IndexByte(kv, '=')
		if i < 0 {
			continue
		}
		key := strings.TrimSpace(kv[:i])
		val := strings.Trim(strings.TrimSpace(kv[i+1:]), `"`)
		switch key {
		case "BANDWIDTH":
			v.Bandwidth, _ = strconv.Atoi(val)
		case "RESOLUTION":
			if x := strings.IndexAny(val, "xX"); x > 0 {
				v.Width, _ = strconv.Atoi(val[:x])
				v.Height, _ = strconv.Atoi(val[x+1:])
			}
		case "CODECS":
			v.Codecs = val
		}
	}
	return v
}

// parseMediaTag parses an #EXT-X-MEDIA attribute list, keeping the raw string so
// a URI rewrite preserves every other attribute (NAME, LANGUAGE, CHANNELS, …).
func parseMediaTag(attrs string) Media {
	m := Media{Attrs: attrs}
	for _, kv := range splitAttrs(attrs) {
		i := strings.IndexByte(kv, '=')
		if i < 0 {
			continue
		}
		key := strings.TrimSpace(kv[:i])
		val := strings.Trim(strings.TrimSpace(kv[i+1:]), `"`)
		switch key {
		case "TYPE":
			m.Type = val
		case "GROUP-ID":
			m.GroupID = val
		case "URI":
			m.URI = val
		}
	}
	return m
}

// setAttr returns the attribute list with key's value replaced by a quoted uri.
// If key isn't present (uri empty), the list is returned unchanged.
func setAttr(attrs, key, uri string) string {
	if uri == "" {
		return attrs
	}
	parts := splitAttrs(attrs)
	found := false
	for i, kv := range parts {
		e := strings.IndexByte(kv, '=')
		if e < 0 {
			continue
		}
		if strings.TrimSpace(kv[:e]) == key {
			parts[i] = key + `="` + uri + `"`
			found = true
		}
	}
	if !found {
		return attrs
	}
	return strings.Join(parts, ",")
}

// splitAttrs splits a comma-separated attribute list, respecting quoted values
// (CODECS can contain commas).
func splitAttrs(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			cur.WriteRune(r)
		case r == ',' && !inQuote:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}
