// Package transcode builds the ffmpeg pipelines that condition media into HLS —
// both the one-time content packaging and the runtime ad conditioning that makes
// server-side ad insertion seamless. Content and the ads spliced into it MUST
// share an encoding Profile (container/codec/resolution/fps/audio) and be
// keyframe-aligned at segment boundaries, or the player stutters at the splice.
//
// This file is the pure, exec-free core: the Profile model, its cache hash, and
// the ffmpeg argument construction. The actual ffmpeg invocation lives in
// runner.go so this stays unit-testable without ffmpeg installed.
package transcode

import (
	"fmt"
	"hash/fnv"
	"path/filepath"
	"strconv"
)

// Container output formats.
const (
	ContainerTS   = "ts"   // MPEG-TS segments (.ts) — simplest for HLS
	ContainerCMAF = "cmaf" // fragmented MP4 (.m4s) — HLS+DASH, needs an init segment
)

// Profile is a target encoding profile. Content is packaged to one Profile per
// ABR rung; an ad is conditioned once per distinct Profile it is asked for. Two
// media that share a Profile can be spliced into one seamless stream.
type Profile struct {
	Container    string // ContainerTS | ContainerCMAF
	VCodec       string // "h264" | "hevc"
	Width        int
	Height       int
	FPS          int
	VBitrateKbps int
	ACodec       string // "aac"
	ASampleRate  int    // e.g. 48000
	ABitrateKbps int
	SegDurSec    int // target HLS segment duration, e.g. 6
}

// DefaultProfile is a safe single-rung 360p H.264 profile used for the MVP
// (P1–P4) before ABR (P5) introduces a rung ladder.
func DefaultProfile() Profile {
	return Profile{
		Container: ContainerTS, VCodec: "h264",
		Width: 640, Height: 360, FPS: 30, VBitrateKbps: 800,
		ACodec: "aac", ASampleRate: 48000, ABitrateKbps: 128, SegDurSec: 6,
	}
}

// Hash is a short, deterministic cache key for the Profile. Conditioned ad
// segments live under ssai/cond/{creative}/{Hash}/, so an ad is transcoded once
// per profile and reused across breaks/viewers.
func (p Profile) Hash() string {
	h := fnv.New64a()
	fmt.Fprintf(h, "%s|%s|%dx%d|%d|%d|%s|%d|%d|%d",
		p.Container, p.VCodec, p.Width, p.Height, p.FPS, p.VBitrateKbps,
		p.ACodec, p.ASampleRate, p.ABitrateKbps, p.SegDurSec)
	return strconv.FormatUint(h.Sum64(), 36)
}

// vcodecLib maps our codec name to the ffmpeg encoder.
func vcodecLib(vcodec string) string {
	if vcodec == "hevc" {
		return "libx265"
	}
	return "libx264"
}

// FFmpegArgs builds the ffmpeg argument list to transcode input → this Profile
// as an HLS VOD playlist in outDir (index.m3u8 + seg_%d.ts). Video is scaled +
// rate-constrained to the profile; keyframes are forced at every segment
// boundary (expr:gte(t,n_forced*SegDur)) so splices land on a keyframe; audio is
// re-encoded to the profile's AAC params. The identical flags are used for both
// content packaging and ad conditioning — that byte-compatibility is the point.
func (p Profile) FFmpegArgs(input, outDir string) []string {
	seg := p.SegDurSec
	if seg <= 0 {
		seg = 6
	}
	segType := "mpegts"
	segName := "seg_%d.ts"
	if p.Container == ContainerCMAF {
		segType = "fmp4"
		segName = "seg_%d.m4s"
	}
	args := []string{
		"-y", "-i", input,
		"-c:v", vcodecLib(p.VCodec),
		"-profile:v", "main",
		"-pix_fmt", "yuv420p",
	}
	if p.VBitrateKbps > 0 {
		args = append(args,
			"-b:v", kbps(p.VBitrateKbps),
			"-maxrate", kbps(p.VBitrateKbps),
			"-bufsize", kbps(2*p.VBitrateKbps),
		)
	}
	if p.Width > 0 && p.Height > 0 {
		args = append(args, "-vf", fmt.Sprintf("scale=%d:%d", p.Width, p.Height))
	}
	if p.FPS > 0 {
		args = append(args, "-r", strconv.Itoa(p.FPS))
	}
	// Keyframe alignment to segment boundaries — the key to a clean splice.
	args = append(args, "-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", seg))
	// Audio.
	args = append(args, "-c:a", "aac")
	if p.ASampleRate > 0 {
		args = append(args, "-ar", strconv.Itoa(p.ASampleRate))
	}
	if p.ABitrateKbps > 0 {
		args = append(args, "-b:a", kbps(p.ABitrateKbps))
	}
	// HLS muxing.
	args = append(args,
		"-f", "hls",
		"-hls_time", strconv.Itoa(seg),
		"-hls_playlist_type", "vod",
		"-hls_segment_type", segType,
		"-hls_flags", "independent_segments",
		"-hls_segment_filename", filepath.Join(outDir, segName),
		filepath.Join(outDir, "index.m3u8"),
	)
	return args
}

func kbps(v int) string { return strconv.Itoa(v) + "k" }
