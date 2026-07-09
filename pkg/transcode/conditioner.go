package transcode

import (
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sync/singleflight"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ssai"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects"
)

// Conditioner turns an auction-winning ad into HLS segments that are byte-
// compatible with a content stream (same Profile), caching the result in the
// object store so each (creative, profile) is transcoded once and reused across
// breaks and viewers. This is the heart of runtime SSAI.
type Conditioner struct {
	Store      objects.Store
	Runner     Runner
	HTTP       *http.Client
	Bucket     string // object-store bucket (e.g. adtech-creatives)
	Prefix     string // key prefix for conditioned ads (e.g. ssai/cond)
	PublicBase string // browser-reachable base for segment URLs (e.g. http://host/v1/creatives)

	sf singleflight.Group
}

// CondSegment is one conditioned ad segment: a browser-reachable URL + duration.
type CondSegment struct {
	URI      string  `json:"uri"`
	Duration float64 `json:"duration"`
}

// Conditioned is the result of conditioning an ad to a Profile.
type Conditioned struct {
	CreativeID  string        `json:"creative_id"`
	ProfileHash string        `json:"profile_hash"`
	Cached      bool          `json:"cached"`
	Duration    float64       `json:"duration"`
	Segments    []CondSegment `json:"segments"`
}

// cacheBase is the object-key prefix a conditioned ad lives under:
// {prefix}/{creativeID}-{contentVersion}/{profileHash}. The contentVersion is a
// short hash of the media URL, so replacing a creative's media (a new asset URL)
// busts the cache instead of serving the stale conditioned ad under the reused
// creative id. (Overwriting the SAME url in place is not detected — version the
// asset URL to force a re-condition.)
func (c *Conditioner) cacheBase(creativeID, mediaURL string, p Profile) string {
	id := creativeID
	if v := contentVersion(mediaURL); v != "" {
		id = creativeID + "-" + v
	}
	return fmt.Sprintf("%s/%s/%s", strings.TrimRight(c.Prefix, "/"), id, p.Hash())
}

func contentVersion(mediaURL string) string {
	if mediaURL == "" {
		return ""
	}
	h := fnv.New32a()
	_, _ = io.WriteString(h, mediaURL)
	return strconv.FormatUint(uint64(h.Sum32()), 36)
}

// Condition returns the conditioned segments for (creativeID, mediaURL, profile),
// doing the transcode only on a cache miss. Concurrent breaks for the same
// ad+media+profile collapse to one transcode via single-flight.
func (c *Conditioner) Condition(ctx context.Context, creativeID, mediaURL string, p Profile) (*Conditioned, error) {
	key := creativeID + "|" + mediaURL + "|" + p.Hash()
	v, err, _ := c.sf.Do(key, func() (interface{}, error) {
		return c.conditionOnce(ctx, creativeID, mediaURL, p)
	})
	if err != nil {
		return nil, err
	}
	return v.(*Conditioned), nil
}

// Cached returns the conditioned ad only if it is already in the store — never
// transcodes. Serving paths (the stitcher) use this so a manifest response never
// blocks on ffmpeg; on a miss they slate and warm asynchronously. Returns
// (nil, nil) when not yet conditioned. mediaURL participates in the cache key so
// a replaced creative isn't served stale.
func (c *Conditioner) Cached(ctx context.Context, creativeID, mediaURL string, p Profile) (*Conditioned, error) {
	base := c.cacheBase(creativeID, mediaURL, p)
	if ok, err := c.Store.Exists(ctx, c.Bucket, base+"/index.m3u8"); err != nil || !ok {
		return nil, err
	}
	return c.fromCache(ctx, creativeID, p.Hash(), base)
}

func (c *Conditioner) conditionOnce(ctx context.Context, creativeID, mediaURL string, p Profile) (*Conditioned, error) {
	ph := p.Hash()
	base := c.cacheBase(creativeID, mediaURL, p)

	// Cache hit: the conditioned playlist already exists — read it back.
	if ok, err := c.Store.Exists(ctx, c.Bucket, base+"/index.m3u8"); err == nil && ok {
		return c.fromCache(ctx, creativeID, ph, base)
	}

	// Miss: fetch the mezzanine, transcode+segment, upload.
	src, err := c.fetchSource(ctx, mediaURL)
	if err != nil {
		return nil, fmt.Errorf("fetch mezzanine: %w", err)
	}
	defer os.Remove(src)

	res, err := c.Runner.Package(ctx, src, p)
	if err != nil {
		return nil, fmt.Errorf("condition transcode: %w", err)
	}
	if err := c.put(ctx, base+"/index.m3u8", []byte(res.Playlist), "application/vnd.apple.mpegurl"); err != nil {
		return nil, fmt.Errorf("upload playlist: %w", err)
	}
	out := &Conditioned{CreativeID: creativeID, ProfileHash: ph, Cached: false, Duration: res.TotalDuration()}
	for _, s := range res.Segments {
		if err := c.put(ctx, base+"/"+s.Name, s.Data, "video/mp2t"); err != nil {
			return nil, fmt.Errorf("upload segment %s: %w", s.Name, err)
		}
		out.Segments = append(out.Segments, CondSegment{URI: c.segURL(base, s.Name), Duration: s.Duration})
	}
	return out, nil
}

// fromCache reads a previously-conditioned playlist and rebuilds the segment
// list (browser URLs + durations) without touching ffmpeg.
func (c *Conditioner) fromCache(ctx context.Context, creativeID, ph, base string) (*Conditioned, error) {
	rc, err := c.Store.Get(ctx, c.Bucket, base+"/index.m3u8")
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	m, err := ssai.ParseMedia(string(body))
	if err != nil {
		return nil, err
	}
	out := &Conditioned{CreativeID: creativeID, ProfileHash: ph, Cached: true}
	for _, s := range m.Segments {
		out.Duration += s.Duration
		out.Segments = append(out.Segments, CondSegment{URI: c.segURL(base, s.URI), Duration: s.Duration})
	}
	return out, nil
}

func (c *Conditioner) segURL(base, name string) string {
	return strings.TrimRight(c.PublicBase, "/") + "/" + base + "/" + name
}

func (c *Conditioner) put(ctx context.Context, key string, data []byte, ct string) error {
	return c.Store.Put(ctx, c.Bucket, key, strings.NewReader(string(data)), int64(len(data)), ct)
}

// fetchSource downloads the mezzanine to a temp file. If it's one of our own
// /v1/creatives proxy URLs, it reads straight from the object store (the
// in-cluster transcoder can't reach the browser-facing gateway host); otherwise
// it HTTP-GETs (a real external CDN mezzanine).
func (c *Conditioner) fetchSource(ctx context.Context, mediaURL string) (string, error) {
	f, err := os.CreateTemp("", "mezz-*")
	if err != nil {
		return "", err
	}
	defer f.Close()

	if key := storeKeyFromURL(mediaURL); key != "" {
		rc, err := c.Store.Get(ctx, c.Bucket, key)
		if err != nil {
			return "", fmt.Errorf("store get %s: %w", key, err)
		}
		defer rc.Close()
		if _, err := io.Copy(f, rc); err != nil {
			return "", err
		}
		return f.Name(), nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, mediaURL, nil)
	if err != nil {
		return "", err
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("mezzanine GET %s: status %d", mediaURL, resp.StatusCode)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// storeKeyFromURL extracts the object key from one of our /v1/creatives proxy
// URLs, or "" if the URL isn't a creatives-proxy URL.
func storeKeyFromURL(u string) string {
	const marker = "/v1/creatives/"
	if i := strings.Index(u, marker); i >= 0 {
		return u[i+len(marker):]
	}
	return ""
}
