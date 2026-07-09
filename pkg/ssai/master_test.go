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
	out := ParseMaster(BuildMaster(in, nil))
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

// demuxedMaster references a separate audio group via AUDIO="aud" — the shape a
// real (non-muxed) ABR master takes.
const demuxedMaster = `#EXTM3U
#EXT-X-VERSION:4
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="English",DEFAULT=YES,LANGUAGE="en",URI="audio/en/index.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=928000,RESOLUTION=640x360,CODECS="avc1.4d401e",AUDIO="aud"
360p/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2928000,RESOLUTION=1280x720,CODECS="avc1.640028",AUDIO="aud"
720p/index.m3u8
`

func TestDemuxedRenditionsPreservedAndRewritable(t *testing.T) {
	renditions := ParseRenditions(demuxedMaster)
	if len(renditions) != 1 {
		t.Fatalf("want 1 rendition, got %d", len(renditions))
	}
	m := renditions[0]
	if m.Type != "AUDIO" || m.GroupID != "aud" || m.URI != "audio/en/index.m3u8" {
		t.Fatalf("rendition parsed wrong: %+v", m)
	}

	// Rewrite the audio-group URI (as serveMaster does) and rebuild.
	variants := ParseMaster(demuxedMaster)
	m.URI = "https://stitch/audio.m3u8"
	out := BuildMaster(variants, []Media{m})

	// The rewritten audio rendition survives with its other attributes intact,
	// and the variants keep their AUDIO="aud" reference (else the player loses
	// audio).
	if !strings.Contains(out, `URI="https://stitch/audio.m3u8"`) {
		t.Errorf("rewritten audio URI missing:\n%s", out)
	}
	if !strings.Contains(out, `NAME="English"`) || !strings.Contains(out, `LANGUAGE="en"`) {
		t.Errorf("rendition attributes dropped on rebuild:\n%s", out)
	}
	if strings.Count(out, `AUDIO="aud"`) != 2 {
		t.Errorf("variants lost their AUDIO group reference:\n%s", out)
	}
	if !strings.Contains(out, "avc1.640028") {
		t.Errorf("variant CODECS dropped on rebuild:\n%s", out)
	}
}
