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

// Variant is one rung of an ABR master playlist.
type Variant struct {
	URI       string
	Bandwidth int    // bits/sec (#EXT-X-STREAM-INF BANDWIDTH)
	Width     int    // optional RESOLUTION width
	Height    int    // optional RESOLUTION height
	Codecs    string // optional CODECS
}

// IsMaster reports whether an HLS playlist is a master (has #EXT-X-STREAM-INF)
// rather than a media playlist (has #EXTINF segments).
func IsMaster(text string) bool {
	return strings.Contains(text, "#EXT-X-STREAM-INF")
}

// ParseMaster extracts the variant rungs from a master playlist. Each
// #EXT-X-STREAM-INF attribute line is followed by the variant playlist URI.
func ParseMaster(text string) []Variant {
	var out []Variant
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var pending *Variant
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			v := parseStreamInf(strings.TrimPrefix(line, "#EXT-X-STREAM-INF:"))
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

// BuildMaster renders a master playlist from variants.
func BuildMaster(variants []Variant) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n")
	for _, v := range variants {
		b.WriteString("#EXT-X-STREAM-INF:BANDWIDTH=")
		b.WriteString(strconv.Itoa(v.Bandwidth))
		if v.Width > 0 && v.Height > 0 {
			fmt.Fprintf(&b, ",RESOLUTION=%dx%d", v.Width, v.Height)
		}
		if v.Codecs != "" {
			fmt.Fprintf(&b, ",CODECS=%q", v.Codecs)
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
