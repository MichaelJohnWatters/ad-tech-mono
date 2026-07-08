package transcode

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ssai"
)

// Segment is one produced HLS media segment: its filename, EXTINF duration, and
// bytes (read back off disk so the caller can upload it to object storage).
type Segment struct {
	Name     string
	Duration float64
	Data     []byte
}

// Result is the HLS output of a package/condition run: the media playlist text
// plus the segments it references, in order.
type Result struct {
	Playlist string
	Segments []Segment
}

// TotalDuration sums the segment durations.
func (r *Result) TotalDuration() float64 {
	var t float64
	for _, s := range r.Segments {
		t += s.Duration
	}
	return t
}

// Runner execs ffmpeg to transcode/segment media into HLS. Used by both content
// packaging (cmd/content-packager) and runtime ad conditioning (cmd/transcoder).
type Runner struct {
	FFmpegPath string        // defaults to "ffmpeg"
	Timeout    time.Duration // per-run wall clock; 0 = 5m
}

func (r Runner) ffmpeg() string {
	if r.FFmpegPath != "" {
		return r.FFmpegPath
	}
	return "ffmpeg"
}

// Available reports whether ffmpeg can be found — callers surface a clear error
// (or skip a test) rather than failing deep in exec.
func (r Runner) Available() bool {
	_, err := exec.LookPath(r.ffmpeg())
	return err == nil
}

// Package transcodes inputPath into an HLS VOD ladder for the given Profile and
// reads the produced playlist + segments back. The temp output dir is cleaned
// up before returning; segment bytes are held in the Result for the caller to
// upload. inputPath must be a local file (the caller downloads a mezzanine URL
// to a temp file first).
func (r Runner) Package(ctx context.Context, inputPath string, p Profile) (*Result, error) {
	if _, err := os.Stat(inputPath); err != nil {
		return nil, fmt.Errorf("transcode input: %w", err)
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	outDir, err := os.MkdirTemp("", "transcode-")
	if err != nil {
		return nil, fmt.Errorf("transcode tmpdir: %w", err)
	}
	defer os.RemoveAll(outDir)

	args := p.FFmpegArgs(inputPath, outDir)
	cmd := exec.CommandContext(ctx, r.ffmpeg(), args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg failed: %w\n%s", err, tail(stderr.Bytes(), 800))
	}
	return readHLSOutput(outDir)
}

// readHLSOutput reads index.m3u8 + its segments from dir. Pure enough to unit-
// test by writing fixture files (no ffmpeg needed). Parses the playlist with
// pkg/ssai so segment order + EXTINF durations come from the same HLS reader the
// stitcher uses.
func readHLSOutput(dir string) (*Result, error) {
	playlist, err := os.ReadFile(filepath.Join(dir, "index.m3u8"))
	if err != nil {
		return nil, fmt.Errorf("read index.m3u8: %w", err)
	}
	m, err := ssai.ParseMedia(string(playlist))
	if err != nil {
		return nil, fmt.Errorf("parse produced playlist: %w", err)
	}
	res := &Result{Playlist: string(playlist)}
	for _, s := range m.Segments {
		data, err := os.ReadFile(filepath.Join(dir, s.URI))
		if err != nil {
			return nil, fmt.Errorf("read segment %s: %w", s.URI, err)
		}
		res.Segments = append(res.Segments, Segment{Name: s.URI, Duration: s.Duration, Data: data})
	}
	if len(res.Segments) == 0 {
		return nil, fmt.Errorf("no segments produced")
	}
	return res, nil
}

func tail(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[len(b)-n:]
}
