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
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ssai"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects"
	objs3 "github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/s3"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/transcode"
)

var log = logger.New("content-packager")

var schema = []config.SchemaEntry{
	{Key: "packager.source_bucket", Type: "string", Tier: config.TierStatic, Default: "adtech-creatives", Description: "Object-store bucket holding the source content MP4 and receiving the packaged HLS.", Service: "content-packager", Since: "v1.6"},
	{Key: "packager.source_key", Type: "string", Tier: config.TierStatic, Default: "media/bbb-720-10mb.mp4", Description: "Object key of the source content MP4 to package.", Service: "content-packager", Since: "v1.6"},
	{Key: "packager.content_id", Type: "string", Tier: config.TierStatic, Default: "sample", Description: "Content id — the packaged HLS lands at {prefix}/{content_id}/index.m3u8.", Service: "content-packager", Since: "v1.6"},
	{Key: "packager.prefix", Type: "string", Tier: config.TierStatic, Default: "ssai/content", Description: "Object-key prefix for the packaged content HLS.", Service: "content-packager", Since: "v1.6"},
	{Key: "packager.break_at_segment", Type: "int", Tier: config.TierStatic, Default: "2", Description: "Segment index where the mid-roll ad break opens (#EXT-X-CUE-OUT).", Service: "content-packager", Since: "v1.6"},
	{Key: "packager.break_segments", Type: "int", Tier: config.TierStatic, Default: "5", Description: "Number of content segments the ad break spans (replaced by the stitched ad).", Service: "content-packager", Since: "v1.6"},
}

func main() {
	sc := config.Setup(constants.ServiceSSAI, schema, log)
	cfg := sc.Cfg
	ctx := context.Background()

	store, bucket := connectStore(cfg)
	if store == nil {
		log.Error("object store unavailable (set s3.endpoint); cannot package content")
		os.Exit(1)
	}
	srcKey := cfg.Get("packager.source_key", "media/bbb-720-10mb.mp4")
	contentID := cfg.Get("packager.content_id", "sample")
	prefix := strings.TrimRight(cfg.Get("packager.prefix", "ssai/content"), "/")
	breakAt := cfg.GetInt("packager.break_at_segment", 2)
	breakSegs := cfg.GetInt("packager.break_segments", 5)

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

	// 2. Segment it to HLS.
	profile := transcode.DefaultProfile()
	res, err := runner.Package(ctx, src, profile)
	if err != nil {
		log.Error("ffmpeg package failed", "error", err)
		os.Exit(1)
	}
	log.Info("content segmented", "segments", len(res.Segments), "duration_s", res.TotalDuration())

	// 3. Insert a mid-roll ad break (real content carries SCTE-35 markers; we
	//    stamp CUE-OUT/CUE-IN so the stitcher has an avail to fill).
	playlist, err := insertBreak(res.Playlist, breakAt, breakSegs, profile.SegDurSec)
	if err != nil {
		log.Error("insert break failed", "error", err)
		os.Exit(1)
	}

	// 4. Upload the playlist + segments (relative seg URIs resolve against the
	//    playlist's URL, so no rewriting needed).
	base := fmt.Sprintf("%s/%s", prefix, contentID)
	if err := put(ctx, store, bucket, base+"/index.m3u8", []byte(playlist), "application/vnd.apple.mpegurl"); err != nil {
		log.Error("upload playlist failed", "error", err)
		os.Exit(1)
	}
	for _, s := range res.Segments {
		if err := put(ctx, store, bucket, base+"/"+s.Name, s.Data, "video/mp2t"); err != nil {
			log.Error("upload segment failed", "seg", s.Name, "error", err)
			os.Exit(1)
		}
	}
	log.Info("content packaged", "bucket", bucket, "playlist", base+"/index.m3u8",
		"segments", len(res.Segments), "break_at", breakAt, "break_segments", breakSegs)
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
	endpoint := cfg.Get("s3.endpoint", "")
	bucket := cfg.Get("packager.source_bucket", "adtech-creatives")
	if endpoint == "" {
		return nil, bucket
	}
	cli, err := objs3.New(objs3.Config{
		Endpoint:  strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://"),
		AccessKey: cfg.Get("s3.access_key", "minioadmin"),
		SecretKey: cfg.Get("s3.secret_key", "minioadmin"),
		UseSSL:    cfg.GetBool("s3.use_ssl", false),
	})
	if err != nil {
		log.Warn("s3 init failed", "error", err)
		return nil, bucket
	}
	return cli, bucket
}
