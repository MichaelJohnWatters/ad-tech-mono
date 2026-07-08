package ssai

import (
	"fmt"
	"strings"
	"testing"
)

const sampleManifest = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:6
#EXT-X-MEDIA-SEQUENCE:0
#EXTINF:6.0,
content_001.ts
#EXTINF:6.0,
content_002.ts
#EXT-X-CUE-OUT:DURATION=30
#EXTINF:6.0,
content_003.ts
#EXTINF:6.0,
content_004.ts
#EXT-X-CUE-IN
#EXTINF:6.0,
content_005.ts
#EXT-X-ENDLIST
`

func TestParseMedia(t *testing.T) {
	m, err := ParseMedia(sampleManifest)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(m.Segments) != 5 {
		t.Fatalf("want 5 segments, got %d", len(m.Segments))
	}
	if !m.EndList {
		t.Error("EndList not detected")
	}
	if len(m.Header) == 0 || m.Header[0] != "#EXTM3U" {
		t.Errorf("header not preserved: %v", m.Header)
	}
	// The cue-out sits on content_003 (index 2), cue-in on content_005 (index 4).
	if m.Segments[2].CueOut != 30 {
		t.Errorf("cue-out duration = %v, want 30", m.Segments[2].CueOut)
	}
	if !m.Segments[4].CueIn {
		t.Errorf("cue-in not on segment 4")
	}
}

func TestBreaks(t *testing.T) {
	m, _ := ParseMedia(sampleManifest)
	breaks := m.Breaks()
	if len(breaks) != 1 {
		t.Fatalf("want 1 break, got %d", len(breaks))
	}
	b := breaks[0]
	// Span covers content_003, content_004 (indices 2,3); cue-in segment 4 is
	// content again and excluded.
	if b.Start != 2 || b.End != 4 || b.Duration != 30 {
		t.Errorf("break span = %+v, want {2 4 30}", b)
	}
}

func TestStitchReplacesContentWithAds(t *testing.T) {
	m, _ := ParseMedia(sampleManifest)
	// Fill the 30s break with 5×6s ad segments.
	m.Stitch(func(i int, span BreakSpan) []Segment {
		return SegmentAds(span.Duration, 6, func(n int, start float64) string {
			return fmt.Sprintf("ad_%d_seg_%d.ts", i, n)
		})
	})

	out := m.Render()

	// Original content-during-break segments must be gone; ad segments present.
	if strings.Contains(out, "content_003.ts") || strings.Contains(out, "content_004.ts") {
		t.Errorf("content-during-break not replaced:\n%s", out)
	}
	if strings.Count(out, "ad_0_seg_") != 5 {
		t.Errorf("want 5 ad segments, got:\n%s", out)
	}
	// Pre/post-break content preserved.
	for _, keep := range []string{"content_001.ts", "content_002.ts", "content_005.ts"} {
		if !strings.Contains(out, keep) {
			t.Errorf("content segment %s dropped:\n%s", keep, out)
		}
	}
	// Discontinuity markers bracket the ad block: one before the first ad and
	// one on the returning content segment.
	if strings.Count(out, "#EXT-X-DISCONTINUITY") != 2 {
		t.Errorf("want 2 discontinuities (into ad, back to content):\n%s", out)
	}
	if m.AdDuration() != 30 {
		t.Errorf("stitched ad duration = %v, want 30", m.AdDuration())
	}
	// Re-parsing the stitched output must still be a valid manifest.
	if _, err := ParseMedia(out); err != nil {
		t.Errorf("stitched manifest does not re-parse: %v", err)
	}
}

func TestStitchNoFillKeepsContent(t *testing.T) {
	m, _ := ParseMedia(sampleManifest)
	m.Stitch(func(i int, span BreakSpan) []Segment { return nil }) // no bid
	out := m.Render()
	if !strings.Contains(out, "content_003.ts") || !strings.Contains(out, "content_004.ts") {
		t.Errorf("no-fill break should keep original content (slate/passthrough):\n%s", out)
	}
}

func TestSegmentAdsRemainder(t *testing.T) {
	// 15s into 6s segments → 6,6,3.
	segs := SegmentAds(15, 6, func(n int, start float64) string { return fmt.Sprintf("s%d", n) })
	if len(segs) != 3 {
		t.Fatalf("want 3 segments, got %d", len(segs))
	}
	if segs[2].Duration != 3 {
		t.Errorf("last segment duration = %v, want 3", segs[2].Duration)
	}
}

func TestMultipleBreaks(t *testing.T) {
	manifest := `#EXTM3U
#EXTINF:6.0,
c1.ts
#EXT-X-CUE-OUT:DURATION=12
#EXTINF:6.0,
c2.ts
#EXT-X-CUE-IN
#EXTINF:6.0,
c3.ts
#EXT-X-CUE-OUT:DURATION=6
#EXTINF:6.0,
c4.ts
#EXT-X-CUE-IN
#EXTINF:6.0,
c5.ts
#EXT-X-ENDLIST
`
	m, _ := ParseMedia(manifest)
	if len(m.Breaks()) != 2 {
		t.Fatalf("want 2 breaks, got %d", len(m.Breaks()))
	}
	m.Stitch(func(i int, span BreakSpan) []Segment {
		return SegmentAds(span.Duration, 6, func(n int, start float64) string {
			return fmt.Sprintf("ad_b%d_s%d.ts", i, n)
		})
	})
	out := m.Render()
	// Break 0 = 12s → 2 segs; break 1 = 6s → 1 seg.
	if strings.Count(out, "ad_b0_s") != 2 || strings.Count(out, "ad_b1_s") != 1 {
		t.Errorf("wrong per-break ad counts:\n%s", out)
	}
	// Both content-during-break segments replaced.
	if strings.Contains(out, "c2.ts") || strings.Contains(out, "c4.ts") {
		t.Errorf("break content not replaced:\n%s", out)
	}
}
