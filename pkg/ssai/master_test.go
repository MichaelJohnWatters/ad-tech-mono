package ssai

import (
	"strings"
	"testing"
)

const sampleMaster = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-STREAM-INF:BANDWIDTH=928000,RESOLUTION=640x360,CODECS="avc1.4d401e,mp4a.40.2"
360p/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=1528000,RESOLUTION=854x480
480p/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2928000,RESOLUTION=1280x720
720p/index.m3u8
`

func TestIsMaster(t *testing.T) {
	if !IsMaster(sampleMaster) {
		t.Error("sample master not detected as master")
	}
	if IsMaster(sampleManifest) {
		t.Error("media playlist wrongly detected as master")
	}
}

func TestParseMaster(t *testing.T) {
	vs := ParseMaster(sampleMaster)
	if len(vs) != 3 {
		t.Fatalf("want 3 variants, got %d", len(vs))
	}
	if vs[0].URI != "360p/index.m3u8" || vs[0].Bandwidth != 928000 || vs[0].Width != 640 || vs[0].Height != 360 {
		t.Errorf("variant 0 wrong: %+v", vs[0])
	}
	if !strings.Contains(vs[0].Codecs, "avc1") {
		t.Errorf("codecs not parsed (quoted commas): %q", vs[0].Codecs)
	}
	if vs[2].Height != 720 {
		t.Errorf("variant 2 height = %d, want 720", vs[2].Height)
	}
}

func TestBuildMasterRoundTrip(t *testing.T) {
	in := []Variant{
		{URI: "360p/index.m3u8", Bandwidth: 928000, Width: 640, Height: 360, Codecs: "avc1.4d401e,mp4a.40.2"},
		{URI: "720p/index.m3u8", Bandwidth: 2928000, Width: 1280, Height: 720},
	}
	out := ParseMaster(BuildMaster(in))
	if len(out) != 2 {
		t.Fatalf("round-trip lost variants: %d", len(out))
	}
	if out[0].URI != in[0].URI || out[0].Bandwidth != in[0].Bandwidth || out[0].Height != in[0].Height {
		t.Errorf("round-trip mismatch: %+v vs %+v", out[0], in[0])
	}
	if !strings.Contains(out[0].Codecs, "avc1") || !strings.Contains(out[0].Codecs, "mp4a") {
		t.Errorf("codecs with comma didn't survive round-trip: %q", out[0].Codecs)
	}
}
