package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects"
)

// sampleMedia is the curated set of video/audio files the demo uses for video
// ad creatives, the content clip, and audio spots. They're pulled once from
// their upstream sources into our own object store so the platform serves them
// itself — no live third-party dependency on every request (test-videos.co.uk /
// soundhelix were external and fragile; Google's sample bucket 403'd). After
// the first seed everything plays from Minio via the gateway /v1/creatives
// proxy. Keys live under media/ in the same public-read creatives bucket.
var sampleMedia = []struct {
	src, key, contentType string
}{
	{"https://test-videos.co.uk/vids/bigbuckbunny/mp4/h264/360/Big_Buck_Bunny_360_10s_2MB.mp4", "media/bbb-360-2mb.mp4", "video/mp4"},
	{"https://test-videos.co.uk/vids/bigbuckbunny/mp4/h264/360/Big_Buck_Bunny_360_10s_5MB.mp4", "media/bbb-360-5mb.mp4", "video/mp4"},
	{"https://test-videos.co.uk/vids/sintel/mp4/h264/360/Sintel_360_10s_1MB.mp4", "media/sintel-360-1mb.mp4", "video/mp4"},
	{"https://test-videos.co.uk/vids/bigbuckbunny/mp4/h264/720/Big_Buck_Bunny_720_10s_10MB.mp4", "media/bbb-720-10mb.mp4", "video/mp4"},
	{"https://www.soundhelix.com/examples/mp3/SoundHelix-Song-1.mp3", "media/audio-1.mp3", "audio/mpeg"},
	{"https://www.soundhelix.com/examples/mp3/SoundHelix-Song-2.mp3", "media/audio-2.mp3", "audio/mpeg"},
}

// uploadSampleMedia fetches each sample once into the object store. Best-effort:
// a file already present is skipped (so re-seeds don't re-download and there's
// no external hit once populated), and a failed fetch is logged but doesn't
// fail the seed — the /v1/media proxy remains as an offline fallback.
func uploadSampleMedia(ctx context.Context, store objects.Store, bucket string, log *slog.Logger) {
	if store == nil {
		log.Warn("object store unavailable, skipping sample media upload; video/audio will fall back to the external /v1/media proxy")
		return
	}
	client := &http.Client{Timeout: 60 * time.Second}
	uploaded, skipped, failed := 0, 0, 0
	for _, m := range sampleMedia {
		if ok, err := store.Exists(ctx, bucket, m.key); err == nil && ok {
			skipped++
			continue
		}
		body, err := fetchMedia(ctx, client, m.src)
		if err != nil {
			log.Warn("sample media fetch failed", "src", m.src, "error", err)
			failed++
			continue
		}
		if err := store.Put(ctx, bucket, m.key, bytes.NewReader(body), int64(len(body)), m.contentType); err != nil {
			log.Warn("sample media upload failed", "key", m.key, "error", err)
			failed++
			continue
		}
		uploaded++
	}
	log.Info("sample media synced to object store", "uploaded", uploaded, "skipped_present", skipped, "failed", failed, "bucket", bucket)
}

func fetchMedia(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}
