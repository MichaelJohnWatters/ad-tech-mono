package transcode

import (
	"fmt"
	"strconv"
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
