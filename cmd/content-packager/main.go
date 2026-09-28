// cmd/content-packager is a one-shot Job that turns a source content MP4 into a
// real segmented HLS VOD in the object store, with an ad-break marker, so the
// SSAI stitcher has genuine content segments to splice conditioned ads into.
//
// Real content is always pre-packaged like this (a streaming service segments
// its catalogue up front). We do it once, into Minio; the SSAI service then
// fetches this as its origin (?origin=<gateway>/v1/creatives/{prefix}/{id}/index.m3u8).
//
// Requires ffmpeg (baked into the image). See docs/SSAI_CONDITIONING.md (P1).
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ssai"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects"
	objs3 "github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/s3"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/transcode"
)

var log = logger.New("content-packager")

func main() {
	sc := config.Setup(constants.ServiceSSAI, keys.ContentPackagerSchema(), log)
	cfg := sc.Cfg
	ctx := context.Background()

	store, bucket := connectStore(cfg)
	if store == nil {
		log.Error("object store unavailable (set s3.endpoint); cannot package content")
		os.Exit(1)
	}
	srcKey := keys.ContentPackager.PackagerSourceKey.Get(cfg)
	contentID := keys.ContentPackager.PackagerContentID.Get(cfg)
	prefix := strings.TrimRight(keys.ContentPackager.PackagerPrefix.Get(cfg), "/")
	breakAt := keys.ContentPackager.PackagerBreakAtSegment.Get(cfg)
	breakSegs := keys.ContentPackager.PackagerBreakSegments.Get(cfg)
	targetDur := keys.ContentPackager.PackagerTargetDuration.Get(cfg)
	breaksSpec := keys.ContentPackager.PackagerBreaks.Get(cfg)
	// Audio mode: package a single audio-only rendition (podcast / streaming
	// radio) instead of the video ABR ladder, and skip the master playlist —
	// audio is single-rendition, so the origin is the media playlist directly.
	audioMode := keys.ContentPackager.PackagerAudio.Get(cfg)

	runner := transcode.Runner{Timeout: 10 * time.Minute}
	if !runner.Available() {
		log.Error("ffmpeg not found; content-packager needs ffmpeg in the image")
		os.Exit(1)
	}

	// 1. Pull the source MP4 to a temp file.
	src, err := download(ctx, store, bucket, srcKey)
	if err != nil {
		log.Error("download source failed", "key", srcKey, "error", err)
		os.Exit(1)
	}
	defer os.Remove(src)

	// 1b. Loop the source up to the target duration so a short demo clip becomes a
	//     longer content stream with room for pre/mid/post ad breaks.
	if targetDur > 0 {
		looped, err := loopSource(ctx, src, targetDur)
		if err != nil {
			log.Error("loop source failed", "target_sec", targetDur, "error", err)
			os.Exit(1)
		}
		defer os.Remove(looped)
		src = looped
		log.Info("source looped", "target_sec", targetDur)
	}

	// 2-4. Package every ABR rung: segment → stamp the mid-roll break → upload,
	//      collecting variants for the master. Real content carries SCTE-35; we
	//      stamp CUE-OUT/CUE-IN so the stitcher has an avail to fill.
	base := fmt.Sprintf("%s/%s", prefix, contentID)
	// CMAF mode packages fMP4 (.m4s + init.mp4) so the SAME segments serve both
	// HLS (with #EXT-X-MAP) and DASH; TS mode is HLS-only.
	cmaf := strings.EqualFold(keys.ContentPackager.PackagerContainer.Get(cfg), "cmaf")
	segCT := "video/mp2t"
	if cmaf {
		segCT = "video/mp4"
	}
	profiles := transcode.ParseLadder(keys.Transcode.Ladder.Get(cfg))
	if audioMode {
		profiles = []transcode.Profile{transcode.DefaultAudioProfile()}
	}
	var variants []ssai.Variant
	for _, profile := range profiles {
		if cmaf {
			profile.Container = transcode.ContainerCMAF
		}
		res, err := runner.Package(ctx, src, profile)
		if err != nil {
			log.Error("ffmpeg package failed", "rung", profile.RungName(), "error", err)
			os.Exit(1)
		}
		playlist, err := insertBreaks(res.Playlist, breaksSpec, breakAt, breakSegs, profile.SegDurSec)
		if err != nil {
			log.Error("insert break failed", "rung", profile.RungName(), "error", err)
			os.Exit(1)
		}
		rung := profile.RungName()
		if audioMode {
			rung = "audio"
		}
		rbase := base + "/" + rung
		if err := put(ctx, store, bucket, rbase+"/index.m3u8", []byte(playlist), "application/vnd.apple.mpegurl"); err != nil {
			log.Error("upload variant playlist failed", "rung", rung, "error", err)
			os.Exit(1)
		}
		// fMP4 init segment (shared by all media segments of the rung).
		if res.Init != nil {
			if err := put(ctx, store, bucket, rbase+"/"+res.Init.Name, res.Init.Data, "video/mp4"); err != nil {
				log.Error("upload init failed", "rung", rung, "error", err)
				os.Exit(1)
			}
		}
		for _, s := range res.Segments {
			if err := put(ctx, store, bucket, rbase+"/"+s.Name, s.Data, segCT); err != nil {
				log.Error("upload segment failed", "rung", rung, "seg", s.Name, "error", err)
				os.Exit(1)
			}
		}
		variants = append(variants, ssai.Variant{
			URI: rung + "/index.m3u8", Bandwidth: profile.BandwidthBps(),
			Width: profile.Width, Height: profile.Height, Codecs: profile.Codecs(),
		})
		log.Info("rung packaged", "rung", rung, "segments", len(res.Segments), "duration_s", res.TotalDuration())
	}

	// Audio is single-rendition: the origin is {base}/audio/index.m3u8, no master.
	if audioMode {
		log.Info("audio content packaged", "bucket", bucket, "origin", base+"/audio/index.m3u8",
			"break_at", breakAt, "break_segments", breakSegs)
		return
	}

	// Master playlist referencing the rungs (relative variant URIs). Muxed A/V
	// variants — no separate audio rendition group.
	master := ssai.BuildMaster(variants, nil)
	if err := put(ctx, store, bucket, base+"/master.m3u8", []byte(master), "application/vnd.apple.mpegurl"); err != nil {
		log.Error("upload master failed", "error", err)
		os.Exit(1)
	}
	log.Info("content packaged", "bucket", bucket, "master", base+"/master.m3u8",
		"rungs", len(variants), "break_at", breakAt, "break_segments", breakSegs)
}

// insertBreak stamps a #EXT-X-CUE-OUT on the segment at breakAt and a
// #EXT-X-CUE-IN on the segment breakSegs later, so the stitcher sees an ad
// avail spanning those content segments.
func insertBreak(playlist string, breakAt, breakSegs, segDur int) (string, error) {
	m, err := ssai.ParseMedia(playlist)
	if err != nil {
		return "", err
	}
	if breakAt < 0 || breakAt >= len(m.Segments) {
		breakAt = 0
	}
	end := breakAt + breakSegs
	if end >= len(m.Segments) {
		end = len(m.Segments) - 1
	}
	if end <= breakAt {
		return playlist, nil // too short to mark a break; leave content as-is
	}
	m.Segments[breakAt].CueOut = float64((end - breakAt) * segDur)
	m.Segments[end].CueIn = true
	return m.Render(), nil
}

// insertBreaks stamps one or more ad-break avails. When spec is empty it falls
// back to the single legacy break (breakAt/breakSegs). A spec like "pre,mid,post"
// stamps a pre-roll (segment 0), a mid-roll (the middle segment), and a
// post-roll (the second-to-last segment) — each a one-segment avail the stitcher
// fills. Positions are derived from the actual segment count so they land in-range.
func insertBreaks(playlist, spec string, breakAt, breakSegs, segDur int) (string, error) {
	if strings.TrimSpace(spec) == "" {
		return insertBreak(playlist, breakAt, breakSegs, segDur)
	}
	m, err := ssai.ParseMedia(playlist)
	if err != nil {
		return "", err
	}
	n := len(m.Segments)
	type bp struct{ at, segs int }
	var positions []bp
	for _, s := range strings.Split(spec, ",") {
		switch strings.TrimSpace(strings.ToLower(s)) {
		case "pre":
			positions = append(positions, bp{0, 1})
		case "mid":
			if n >= 3 {
				positions = append(positions, bp{n / 2, 1})
			}
		case "post":
			if n >= 3 {
				positions = append(positions, bp{n - 2, 1})
			}
		}
	}
	for _, p := range positions {
		at, end := p.at, p.at+p.segs
		if at < 0 || at >= n {
			continue
		}
		if end >= n {
			end = n - 1
		}
		if end <= at {
			continue
		}
		m.Segments[at].CueOut = float64((end - at) * segDur)
		m.Segments[end].CueIn = true
	}
	return m.Render(), nil
}

// loopSource loops the source MP4 up to targetSec seconds (re-encoding so the
// looped boundaries are clean for downstream per-rung segmenting) and returns the
// path to the looped temp file.
func loopSource(ctx context.Context, src string, targetSec int) (string, error) {
	out := src + ".looped.mp4"
	cmd := exec.CommandContext(ctx, "ffmpeg", "-y",
		"-stream_loop", "-1", "-i", src,
		"-t", strconv.Itoa(targetSec),
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-movflags", "+faststart", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("ffmpeg loop: %w\n%s", err, b)
	}
	return out, nil
}

func download(ctx context.Context, store objects.Store, bucket, key string) (string, error) {
	rc, err := store.Get(ctx, bucket, key)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	f, err := os.CreateTemp("", "content-*.mp4")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(f, rc); err != nil {
		return "", err
	}
	return f.Name(), nil
}

func put(ctx context.Context, store objects.Store, bucket, key string, data []byte, ct string) error {
	return store.Put(ctx, bucket, key, strings.NewReader(string(data)), int64(len(data)), ct)
}

func connectStore(cfg *config.Config) (objects.Store, string) {
	endpoint := cfg.Get(keys.S3.Endpoint.Key(), "")
	bucket := keys.ContentPackager.PackagerSourceBucket.Get(cfg)
	if endpoint == "" {
		return nil, bucket
	}
	cli, err := objs3.New(objs3.Config{
		Endpoint:  strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://"),
		AccessKey: keys.S3.AccessKey.Get(cfg),
		SecretKey: keys.S3.SecretKey.Get(cfg),
		UseSSL:    keys.S3.UseSSL.Get(cfg),
	})
	if err != nil {
		log.Warn("s3 init failed", "error", err)
		return nil, bucket
	}
	return cli, bucket
}
