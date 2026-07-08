package transcode

import (
	"strings"
	"testing"
)

func argVal(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func TestProfileHashStableAndDistinct(t *testing.T) {
	p := DefaultProfile()
	if p.Hash() != p.Hash() {
		t.Fatal("hash not stable")
	}
	q := p
	q.VBitrateKbps = 1200
	if p.Hash() == q.Hash() {
		t.Error("different bitrate must hash differently (else cache collides across rungs)")
	}
	r := p
	r.Width = 1280
	r.Height = 720
	if p.Hash() == r.Hash() {
		t.Error("different resolution must hash differently")
	}
}

func TestFFmpegArgsTS(t *testing.T) {
	p := DefaultProfile()
	args := p.FFmpegArgs("/tmp/in.mp4", "/out")

	if argVal(args, "-i") != "/tmp/in.mp4" {
		t.Errorf("input not set: %v", args)
	}
	if argVal(args, "-c:v") != "libx264" {
		t.Errorf("h264 must map to libx264, got %q", argVal(args, "-c:v"))
	}
	if argVal(args, "-b:v") != "800k" {
		t.Errorf("video bitrate = %q, want 800k", argVal(args, "-b:v"))
	}
	if argVal(args, "-vf") != "scale=640:360" {
		t.Errorf("scale = %q", argVal(args, "-vf"))
	}
	if argVal(args, "-r") != "30" {
		t.Errorf("fps = %q", argVal(args, "-r"))
	}
	// Keyframe alignment to the 6s segment boundary — the seamless-splice key.
	if kf := argVal(args, "-force_key_frames"); kf != "expr:gte(t,n_forced*6)" {
		t.Errorf("keyframe expr = %q", kf)
	}
	if argVal(args, "-hls_time") != "6" {
		t.Errorf("hls_time = %q", argVal(args, "-hls_time"))
	}
	if argVal(args, "-hls_segment_type") != "mpegts" {
		t.Errorf("segment type = %q, want mpegts", argVal(args, "-hls_segment_type"))
	}
	if argVal(args, "-c:a") != "aac" || argVal(args, "-ar") != "48000" || argVal(args, "-b:a") != "128k" {
		t.Errorf("audio args wrong: %v", args)
	}
	// Output playlist + segment pattern in outDir.
	last := args[len(args)-1]
	if !strings.HasSuffix(last, "/out/index.m3u8") {
		t.Errorf("last arg should be the output playlist, got %q", last)
	}
	if sn := argVal(args, "-hls_segment_filename"); !strings.HasSuffix(sn, "/out/seg_%d.ts") {
		t.Errorf("segment filename = %q", sn)
	}
}

func TestFFmpegArgsHEVCandCMAF(t *testing.T) {
	p := DefaultProfile()
	p.VCodec = "hevc"
	p.Container = ContainerCMAF
	args := p.FFmpegArgs("in.mp4", "o")
	if argVal(args, "-c:v") != "libx265" {
		t.Errorf("hevc must map to libx265, got %q", argVal(args, "-c:v"))
	}
	if argVal(args, "-hls_segment_type") != "fmp4" {
		t.Errorf("cmaf must use fmp4 segments, got %q", argVal(args, "-hls_segment_type"))
	}
	if sn := argVal(args, "-hls_segment_filename"); !strings.HasSuffix(sn, "seg_%d.m4s") {
		t.Errorf("cmaf segment name = %q, want .m4s", sn)
	}
}
