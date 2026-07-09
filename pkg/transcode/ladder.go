package transcode

import (
	"fmt"
	"strconv"
	"strings"
)

// The ABR ladder — the set of bitrate rungs content is packaged to and ads are
// conditioned to match. The player adapts across rungs; each rung is a distinct
// Profile, so an ad is conditioned once per rung.

// DefaultLadder is a 3-rung H.264 ladder (360p / 480p / 720p). DefaultProfile is
// the low rung, so single-profile callers (MVP) and the ladder agree on 360p.
func DefaultLadder() []Profile {
	base := DefaultProfile()
	mk := func(w, h, vk int) Profile {
		p := base
		p.Width, p.Height, p.VBitrateKbps = w, h, vk
		return p
	}
	return []Profile{
		mk(640, 360, 800),
		mk(854, 480, 1400),
		mk(1280, 720, 2800),
	}
}

// ParseLadder builds an ABR ladder from a compact spec so the rungs are config-
// driven rather than hardcoded: comma-separated "WxH@vbitrateKbps" entries (e.g.
// "640x360@800,1280x720@2800"), each inheriting the codec/fps/audio/segment
// params of DefaultProfile. An empty or unparseable spec falls back to
// DefaultLadder, so a bad config value can never break packaging/conditioning.
//
// NB: content and the ads spliced into it must share the ladder — if you change
// this, re-run the content-packager so origins are re-segmented to the new rungs.
func ParseLadder(spec string) []Profile {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return DefaultLadder()
	}
	base := DefaultProfile()
	var out []Profile
	for _, rung := range strings.Split(spec, ",") {
		rung = strings.TrimSpace(rung)
		if rung == "" {
			continue
		}
		dims, bitrate := rung, ""
		if at := strings.IndexByte(rung, '@'); at >= 0 {
			dims, bitrate = rung[:at], rung[at+1:]
		}
		x := strings.IndexAny(dims, "xX")
		if x < 0 {
			continue
		}
		w, err1 := strconv.Atoi(strings.TrimSpace(dims[:x]))
		h, err2 := strconv.Atoi(strings.TrimSpace(dims[x+1:]))
		if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
			continue
		}
		p := base
		p.Width, p.Height = w, h
		if vb, err := strconv.Atoi(strings.TrimSpace(bitrate)); err == nil && vb > 0 {
			p.VBitrateKbps = vb
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return DefaultLadder()
	}
	return out
}

// RungName is the human/URL label for a profile's rung, e.g. "360p".
func (p Profile) RungName() string { return strconv.Itoa(p.Height) + "p" }

// BandwidthBps is the advertised master-playlist bandwidth (video+audio bits/s).
func (p Profile) BandwidthBps() int { return (p.VBitrateKbps + p.ABitrateKbps) * 1000 }

// Codecs is the HLS CODECS attribute for the profile (RFC 6381). Main-profile
// H.264 + AAC-LC for the default; HEVC callers get an hvc1 tag; audio-only
// profiles advertise just AAC-LC.
func (p Profile) Codecs() string {
	if p.AudioOnly {
		return "mp4a.40.2"
	}
	v := "avc1.4d401e" // H.264 Main@3.0
	if p.VCodec == "hevc" {
		v = "hvc1.1.6.L93.B0"
	}
	return fmt.Sprintf("%s,mp4a.40.2", v)
}
