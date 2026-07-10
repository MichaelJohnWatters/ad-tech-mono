package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ssai"
)

func plainPlaylist(n int) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:6\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "#EXTINF:6.0,\nseg_%d.ts\n", i)
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

func TestInsertBreak(t *testing.T) {
	out, err := insertBreak(plainPlaylist(8), 2, 3, 6)
	if err != nil {
		t.Fatalf("insertBreak: %v", err)
	}
	m, err := ssai.ParseMedia(out)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	breaks := m.Breaks()
	if len(breaks) != 1 {
		t.Fatalf("want 1 break, got %d", len(breaks))
	}
	b := breaks[0]
	// CUE-OUT on seg 2, CUE-IN on seg 5 → span [2,5), duration 3×6=18.
	if b.Start != 2 || b.End != 5 || b.Duration != 18 {
		t.Errorf("break = %+v, want {2 5 18}", b)
	}
}

func TestInsertBreakClampsShortPlaylist(t *testing.T) {
	// break_segments larger than the playlist → clamp, still a valid break or
	// unchanged content, never a panic.
	out, err := insertBreak(plainPlaylist(3), 2, 10, 6)
	if err != nil {
		t.Fatalf("insertBreak: %v", err)
	}
	if _, err := ssai.ParseMedia(out); err != nil {
		t.Errorf("clamped playlist invalid: %v", err)
	}
}
