package transcode

import (
	"context"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/fs"
)

func TestStoreKeyFromURL(t *testing.T) {
	cases := map[string]string{
		"http://localhost:8080/v1/creatives/media/bbb.mp4":   "media/bbb.mp4",
		"https://host/v1/creatives/ssai/cond/cr/ph/seg_0.ts": "ssai/cond/cr/ph/seg_0.ts",
		"http://cdn.example.com/some/external/ad.mp4":        "",
	}
	for u, want := range cases {
		if got := storeKeyFromURL(u); got != want {
			t.Errorf("storeKeyFromURL(%q) = %q, want %q", u, got, want)
		}
	}
}

// TestConditionerCacheHit exercises the cache path with a filesystem store — no
// ffmpeg: a conditioned playlist already present is read back into the segment
// list with correct browser URLs, and marked cached.
func TestConditionerCacheHit(t *testing.T) {
	store, err := fs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	bucket := "adtech-creatives"
	if err := store.EnsureBucket(ctx, bucket); err != nil {
		t.Fatal(err)
	}

	c := &Conditioner{Store: store, Bucket: bucket, Prefix: "ssai/cond", PublicBase: "http://host/v1/creatives"}
	p := DefaultProfile()
	mediaURL := "http://host/v1/creatives/media/ad.mp4"
	base := c.cacheBase("cr-1", mediaURL, p)

	// Pre-seed a conditioned playlist + segments (simulating a prior transcode).
	playlist := "#EXTM3U\n#EXTINF:6.0,\nseg_0.ts\n#EXTINF:6.0,\nseg_1.ts\n#EXT-X-ENDLIST\n"
	put(t, store, ctx, bucket, base+"/index.m3u8", playlist)
	put(t, store, ctx, bucket, base+"/seg_0.ts", "aa")
	put(t, store, ctx, bucket, base+"/seg_1.ts", "bb")

	out, err := c.Condition(ctx, "cr-1", mediaURL, p)
	if err != nil {
		t.Fatalf("Condition: %v", err)
	}
	if !out.Cached {
		t.Error("want cached=true for a pre-seeded conditioned ad")
	}
	if len(out.Segments) != 2 || out.Duration != 12 {
		t.Fatalf("segments/duration wrong: %+v", out)
	}
	want := "http://host/v1/creatives/" + base + "/seg_0.ts"
	if out.Segments[0].URI != want {
		t.Errorf("segment URI = %q, want %q", out.Segments[0].URI, want)
	}

	// Cache-bust: the same creative id with a REPLACED media URL must not hit the
	// stale cache (Cached returns nil), so it re-conditions instead of serving the
	// old ad.
	got, err := c.Cached(ctx, "cr-1", "http://host/v1/creatives/media/ad-v2.mp4", p)
	if err != nil {
		t.Fatalf("Cached (replaced media): %v", err)
	}
	if got != nil {
		t.Error("replaced-media creative served the stale cache; want a miss (cache-bust)")
	}
	// The original media still hits.
	if hit, _ := c.Cached(ctx, "cr-1", mediaURL, p); hit == nil {
		t.Error("original media should still be a cache hit")
	}
}

func put(t *testing.T, store *fs.Store, ctx context.Context, bucket, key, body string) {
	t.Helper()
	if err := store.Put(ctx, bucket, key, strings.NewReader(body), int64(len(body)), ""); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}
