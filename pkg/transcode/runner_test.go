package transcode

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestReadHLSOutput exercises the read-back/parse logic with fixture files, so
// it runs without ffmpeg installed.
func TestReadHLSOutput(t *testing.T) {
	dir := t.TempDir()
	playlist := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:6\n" +
		"#EXTINF:6.0,\nseg_0.ts\n#EXTINF:6.0,\nseg_1.ts\n#EXTINF:3.0,\nseg_2.ts\n#EXT-X-ENDLIST\n"
	mustWrite(t, filepath.Join(dir, "index.m3u8"), playlist)
	mustWrite(t, filepath.Join(dir, "seg_0.ts"), "aaa")
	mustWrite(t, filepath.Join(dir, "seg_1.ts"), "bbbb")
	mustWrite(t, filepath.Join(dir, "seg_2.ts"), "cc")

	res, err := readHLSOutput(dir)
	if err != nil {
		t.Fatalf("readHLSOutput: %v", err)
	}
	if len(res.Segments) != 3 {
		t.Fatalf("want 3 segments, got %d", len(res.Segments))
	}
	if res.Segments[0].Name != "seg_0.ts" || string(res.Segments[0].Data) != "aaa" {
		t.Errorf("segment 0 wrong: %+v", res.Segments[0])
	}
	if res.TotalDuration() != 15.0 {
		t.Errorf("total duration = %v, want 15", res.TotalDuration())
	}
}

// TestReadHLSOutputFMP4 asserts fMP4/CMAF output: the #EXT-X-MAP init segment is
// captured into Result.Init alongside the .m4s media segments.
func TestReadHLSOutputFMP4(t *testing.T) {
	dir := t.TempDir()
	playlist := "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:6\n" +
		"#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:6.0,\nseg_0.m4s\n#EXTINF:6.0,\nseg_1.m4s\n#EXT-X-ENDLIST\n"
	mustWrite(t, filepath.Join(dir, "index.m3u8"), playlist)
	mustWrite(t, filepath.Join(dir, "init.mp4"), "INIT")
	mustWrite(t, filepath.Join(dir, "seg_0.m4s"), "aa")
	mustWrite(t, filepath.Join(dir, "seg_1.m4s"), "bb")

	res, err := readHLSOutput(dir)
	if err != nil {
		t.Fatalf("readHLSOutput: %v", err)
	}
	if res.Init == nil || res.Init.Name != "init.mp4" || string(res.Init.Data) != "INIT" {
		t.Fatalf("init segment not captured: %+v", res.Init)
	}
	if len(res.Segments) != 2 {
		t.Errorf("want 2 media segments, got %d", len(res.Segments))
	}
}

func TestReadHLSOutputNoSegments(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "index.m3u8"), "#EXTM3U\n#EXT-X-ENDLIST\n")
	if _, err := readHLSOutput(dir); err == nil {
		t.Error("want error when the playlist has no segments")
	}
}

// TestPackageReal runs a genuine ffmpeg transcode when ffmpeg is present (CI /
// the transcoder image); skips otherwise so the unit suite stays green locally.
func TestPackageReal(t *testing.T) {
	r := Runner{}
	if !r.Available() {
		t.Skip("ffmpeg not installed; skipping real transcode")
	}
	// Generate a 12s test pattern to a temp mp4, then package it.
	src := filepath.Join(t.TempDir(), "src.mp4")
	gen := exec.Command(r.ffmpeg(), "-y", "-f", "lavfi", "-i", "testsrc=duration=12:size=320x240:rate=30",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=12", "-c:v", "libx264", "-c:a", "aac", src)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("generate fixture: %v\n%s", err, out)
	}
	res, err := r.Package(context.Background(), src, DefaultProfile())
	if err != nil {
		t.Fatalf("Package: %v", err)
	}
	if len(res.Segments) < 2 {
		t.Errorf("expected multiple 6s segments from a 12s clip, got %d", len(res.Segments))
	}
	if res.TotalDuration() < 10 {
		t.Errorf("total duration = %v, want ~12", res.TotalDuration())
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
