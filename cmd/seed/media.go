package main

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects"
)

// adVideoMP4 is a locally-generated "AD" card (big "AD" on a red background) used
// as the VIDEO ad creative so a served ad is VISUALLY DISTINCT from the Big Buck
// Bunny content clip. Previously both the content clip and the video creatives
// were Big Buck Bunny, so a played pre-roll looked identical to the content and
// the demo appeared to "just play content". Embedded (not fetched) so it's always
// present after a seed with no external dependency. Regenerate: see assets/.
//
//go:embed assets/ad-video.mp4
var adVideoMP4 []byte

// adVideoKey is the object-store key the DSP creative profiles point their video
// media_url at (http://<base>/v1/creatives/media/ad-video.mp4).
const adVideoKey = "media/ad-video.mp4"

// adAudioMP3 is a locally-generated SPOKEN ad spot ("This is an advertisement…
// AdTech Mono…") used as the AUDIO ad creative so a served audio ad is AUDIBLY
// DISTINCT from the (music) podcast/stream content. Previously the audio
// creatives were SoundHelix music tracks, so a played audio ad sounded just like
// the content and the demo appeared to "just play music". Embedded (not fetched)
// so it's always present after a seed with no external dependency. Regenerate:
// see assets/ (macOS `say` → ffmpeg, a sine sting + synthesized speech).
//
//go:embed assets/ad-audio.mp3
var adAudioMP3 []byte

// adAudioKey is the object-store key the DSP creative profiles point their audio
// media_url at (http://<base>/v1/creatives/media/ad-audio.mp3).
const adAudioKey = "media/ad-audio.mp3"

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
	// The generated "AD" video creative is embedded (not fetched) — upload it
	// first, always overwriting so a regenerated card propagates on the next seed.
	if err := store.Put(ctx, bucket, adVideoKey, bytes.NewReader(adVideoMP4), int64(len(adVideoMP4)), "video/mp4"); err != nil {
		log.Warn("ad-video creative upload failed", "key", adVideoKey, "error", err)
	} else {
		log.Info("ad-video creative synced", "key", adVideoKey, "bytes", len(adVideoMP4), "bucket", bucket)
	}
	// Same for the generated spoken AUDIO ad spot — always overwrite so a
	// regenerated spot propagates on the next seed.
	if err := store.Put(ctx, bucket, adAudioKey, bytes.NewReader(adAudioMP3), int64(len(adAudioMP3)), "audio/mpeg"); err != nil {
		log.Warn("ad-audio creative upload failed", "key", adAudioKey, "error", err)
	} else {
		log.Info("ad-audio creative synced", "key", adAudioKey, "bytes", len(adAudioMP3), "bucket", bucket)
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
