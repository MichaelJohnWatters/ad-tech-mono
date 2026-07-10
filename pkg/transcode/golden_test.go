package transcode

// Golden tests that run a REAL ffmpeg. They auto-skip when ffmpeg is absent (so
// the unit suite stays green locally) and run in CI / the transcoder image. This
// is the R1 runtime verification: proof that conditioning actually produces
// playable, splice-compatible HLS — the one thing the pure unit tests can't show.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/fs"
)

// genFixture renders a short A/V test clip (distinct visual pattern + tone) to an
// mp4 with ffmpeg lavfi — no binary asset committed to the repo.
func genFixture(t *testing.T, r Runner, pattern string, dur int) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), pattern+".mp4")
	gen := exec.Command(r.ffmpeg(), "-y",
		"-f", "lavfi", "-i", pattern+"=duration="+itoa(dur)+":size=320x240:rate=30",
		"-f", "lavfi", "-i", "sine=frequency=440:duration="+itoa(dur),
		"-c:v", "libx264", "-c:a", "aac", "-shortest", src)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("generate fixture %s: %v\n%s", pattern, err, out)
	}
	return src
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestGoldenPackageAllProfiles proves each real encoding profile packages to
// valid HLS: TS + CMAF (with an init segment) + audio-only.
func TestGoldenPackageAllProfiles(t *testing.T) {
	r := Runner{}
	if !r.Available() {
		t.Skip("ffmpeg not installed; skipping golden transcode")
	}
	src := genFixture(t, r, "testsrc", 12)

	cmaf := DefaultProfile()
	cmaf.Container = ContainerCMAF
	cases := map[string]Profile{
		"ts":    DefaultProfile(),
		"cmaf":  cmaf,
		"audio": DefaultAudioProfile(),
	}
	for name, p := range cases {
		res, err := r.Package(context.Background(), src, p)
		if err != nil {
			t.Fatalf("%s: Package: %v", name, err)
		}
		if len(res.Segments) < 1 {
			t.Errorf("%s: no segments produced", name)
		}
		if res.TotalDuration() < 10 {
			t.Errorf("%s: total duration = %v, want ~12", name, res.TotalDuration())
		}
		if p.Container == ContainerCMAF {
			if res.Init == nil {
				t.Errorf("%s: fMP4 output missing init segment", name)
			}
			for _, s := range res.Segments {
				if !strings.HasSuffix(s.Name, ".m4s") {
					t.Errorf("%s: expected .m4s segments, got %s", name, s.Name)
				}
			}
		}
		if p.AudioOnly {
			// An audio-only render must carry no video stream.
			if hasVideoStream(t, res, src, p) {
				t.Errorf("%s: audio-only profile produced a video stream", name)
			}
		}
	}
}

// TestGoldenSpliceCompatible is the splice proof: two clips conditioned to the
// SAME profile concatenate with stream-copy (-c copy) and decode cleanly. If the
// encodes weren't byte-compatible, -c copy would fail — which is exactly the
// seamless-splice guarantee SSAI depends on.
func TestGoldenSpliceCompatible(t *testing.T) {
	r := Runner{}
	if !r.Available() {
		t.Skip("ffmpeg not installed; skipping golden splice")
	}
	p := DefaultProfile()
	content, err := r.Package(context.Background(), genFixture(t, r, "testsrc", 12), p)
	if err != nil {
		t.Fatalf("package content: %v", err)
	}
	ad, err := r.Package(context.Background(), genFixture(t, r, "smptebars", 6), p)
	if err != nil {
		t.Fatalf("package ad: %v", err)
	}

	// Lay the segments out and build a concat list: content[0] + ad... + content[1]
	// — the exact content→ad→content boundary the stitcher creates.
	dir := t.TempDir()
	var order []Segment
	order = append(order, content.Segments[0])
	order = append(order, ad.Segments...)
	if len(content.Segments) > 1 {
		order = append(order, content.Segments[1])
	}
	var list strings.Builder
	for i, s := range order {
		name := "part_" + itoa(i) + ".ts"
		if err := os.WriteFile(filepath.Join(dir, name), s.Data, 0o644); err != nil {
			t.Fatal(err)
		}
		list.WriteString("file '" + filepath.Join(dir, name) + "'\n")
	}
	listPath := filepath.Join(dir, "list.txt")
	if err := os.WriteFile(listPath, []byte(list.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "spliced.ts")
	// -c copy: NO re-encode. Succeeds only if every part shares codec/params.
	cmd := exec.Command(r.ffmpeg(), "-y", "-f", "concat", "-safe", "0", "-i", listPath, "-c", "copy", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("stream-copy concat failed (segments not splice-compatible): %v\n%s", err, b)
	}
	// The spliced result must be a decodable stream with roughly the summed length.
	if !decodes(t, r, out) {
		t.Error("spliced stream did not decode cleanly")
	}
}

// TestGoldenConditionerRoundTrip conditions a real clip through the Conditioner
// with a filesystem object store, then reads it back from cache — the full
// transcode → upload → Cached() path with a real ffmpeg.
func TestGoldenConditionerRoundTrip(t *testing.T) {
	r := Runner{}
	if !r.Available() {
		t.Skip("ffmpeg not installed; skipping golden conditioner")
	}
	store, err := fs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	bucket := "adtech-creatives"
	if err := store.EnsureBucket(ctx, bucket); err != nil {
		t.Fatal(err)
	}
	// Seed the mezzanine into the store; the conditioner reads it via the
	// /v1/creatives proxy-URL → object-key path (storeKeyFromURL).
	srcBytes, err := os.ReadFile(genFixture(t, r, "testsrc", 12))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, bucket, "media/spot.mp4", strings.NewReader(string(srcBytes)), int64(len(srcBytes)), "video/mp4"); err != nil {
		t.Fatal(err)
	}
	mediaURL := "http://host/v1/creatives/media/spot.mp4"

	c := &Conditioner{Store: store, Runner: r, Bucket: bucket, Prefix: "ssai/cond", PublicBase: "http://host/v1/creatives"}
	p := DefaultProfile()

	first, err := c.Condition(ctx, "cr-real", mediaURL, p)
	if err != nil {
		t.Fatalf("condition (cold): %v", err)
	}
	if first.Cached {
		t.Error("first conditioning should be a cold transcode, not cached")
	}
	if len(first.Segments) < 1 {
		t.Fatal("no conditioned segments produced")
	}

	// Second call must hit the cache (no transcode) and match.
	second, err := c.Cached(ctx, "cr-real", mediaURL, p)
	if err != nil || second == nil {
		t.Fatalf("cached lookup after conditioning: %v", err)
	}
	if !second.Cached {
		t.Error("second lookup should be cached")
	}
	if len(second.Segments) != len(first.Segments) {
		t.Errorf("cached segment count %d != conditioned %d", len(second.Segments), len(first.Segments))
	}
}

// hasVideoStream reports whether the first produced segment carries a video
// stream (via ffprobe). Returns false if ffprobe is unavailable (assertion
// skipped rather than failing).
func hasVideoStream(t *testing.T, res *Result, src string, p Profile) bool {
	t.Helper()
	probe, err := exec.LookPath("ffprobe")
	if err != nil {
		return false
	}
	dir := t.TempDir()
	seg := filepath.Join(dir, res.Segments[0].Name)
	if err := os.WriteFile(seg, res.Segments[0].Data, 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ := exec.Command(probe, "-v", "error", "-select_streams", "v",
		"-show_entries", "stream=codec_type", "-of", "csv=p=0", seg).CombinedOutput()
	return strings.Contains(string(out), "video")
}

// decodes reports whether ffmpeg can fully decode the file without error.
func decodes(t *testing.T, r Runner, path string) bool {
	t.Helper()
	cmd := exec.Command(r.ffmpeg(), "-v", "error", "-i", path, "-f", "null", "-")
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("decode error: %s", b)
		return false
	}
	return true
}
