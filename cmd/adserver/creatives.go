package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache/warm"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects"
)

// CreativeResolver returns a fully rendered AdCreative for a given creative
// UUID, drawing on:
//
//  1. The warm cache of approved creative metadata (Postgres → memory)
//  2. Inline html_content if the seed put the HTML in the column
//  3. Minio at asset_url for large assets (images/video), with an L1 body cache
//
// The bid handler used to seed creatives in Go and hold them in a map; the
// runtime source of truth is now Postgres, with Minio providing the body when
// the creative is too big for the html_content column.
type CreativeResolver struct {
	meta      *warm.Cache[models.Creative]
	objects   objects.Store
	bucket    string
	bodyTTLFn func() time.Duration
	clk       clock.Clock
	log       *slog.Logger

	mu     sync.RWMutex
	bodies map[string]cachedBody
}

type cachedBody struct {
	html    string
	expires time.Time
}

// bodyTTLFn is called on each cache write so live edits to
// adserver.default_creative_ttl take effect on the next miss without a
// pod restart.
func NewCreativeResolver(meta *warm.Cache[models.Creative], obj objects.Store, bucket string, bodyTTLFn func() time.Duration, clk clock.Clock, log *slog.Logger) *CreativeResolver {
	return &CreativeResolver{
		meta: meta, objects: obj, bucket: bucket,
		bodyTTLFn: bodyTTLFn, clk: clk, log: log,
		bodies: map[string]cachedBody{},
	}
}

// Get returns the ready-to-render creative for id, or (zero, false) if no
// metadata is known. HTML is resolved in order: html_content → Minio body → empty.
func (r *CreativeResolver) Get(ctx context.Context, id string) (AdCreative, bool) {
	row, ok := r.meta.ByID(id)
	if !ok {
		return AdCreative{}, false
	}
	c := AdCreative{
		ID:         row.ID,
		Name:       row.Name,
		Width:      row.Width,
		Height:     row.Height,
		Format:     row.Format,
		LandingURL: row.LandingURL,
	}
	switch {
	case row.HTMLContent != "":
		c.HTML = row.HTMLContent
	case row.AssetURL != "":
		c.HTML = r.fetchBody(ctx, row.AssetURL)
	}
	return c, true
}

// fetchBody pulls a creative body from object storage. Keyed by asset URL so
// callers don't have to convert between bucket+key. Body cache is small (the
// number of approved creatives), TTL keeps it fresh against object overwrites.
func (r *CreativeResolver) fetchBody(ctx context.Context, assetURL string) string {
	r.mu.RLock()
	if entry, ok := r.bodies[assetURL]; ok && r.clk.Now().Before(entry.expires) {
		r.mu.RUnlock()
		return entry.html
	}
	r.mu.RUnlock()

	bucket, key := r.bucket, strings.TrimPrefix(assetURL, "/")
	rc, err := r.objects.Get(ctx, bucket, key)
	if err != nil {
		r.log.Warn("creative body fetch failed", "asset", assetURL, "error", err)
		return ""
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		r.log.Warn("creative body read failed", "asset", assetURL, "error", err)
		return ""
	}
	html := string(body)
	r.mu.Lock()
	r.bodies[assetURL] = cachedBody{html: html, expires: r.clk.Now().Add(r.bodyTTLFn())}
	r.mu.Unlock()
	return html
}

// ListIDs returns every metadata ID currently in the cache. Used by /creatives
// for debugging and to warm-start the bandit at boot.
func (r *CreativeResolver) ListIDs() []string {
	all := r.meta.All()
	ids := make([]string, 0, len(all))
	for _, c := range all {
		ids = append(ids, c.ID)
	}
	return ids
}

// MetaCache exposes the underlying warm cache for richer dump endpoints
// that need full Creative records (pub sim Creatives panel). Read-only;
// callers should not mutate the snapshot.
func (r *CreativeResolver) MetaCache() *warm.Cache[models.Creative] {
	return r.meta
}
