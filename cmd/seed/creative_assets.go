// Minio asset upload for the "image" half of the demo creatives.
// Half of the seed creatives use inline html_content (themed SVG
// embedded in the DB row, no roundtrip — see creative_templates.go);
// the other half use creatives.asset_url, which points at a Minio
// object the gateway proxies for the browser via /v1/creatives/*.
//
// Asset selection: deterministic by creative_id parity. Even-suffix
// creatives (cr-…-001, …-003) stay on the html_content path; odd-
// suffix (…-002, …-004) flip to the asset_url path. Same theme map
// as the HTML side so the two halves look like the same brand
// family, just delivered via different mechanics.
package main

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"log/slog"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects"
)

//go:embed themes/retail-300x250.svg
var creativeRetailSVG []byte

//go:embed themes/tech-300x250.svg
var creativeTechSVG []byte

//go:embed themes/auto-300x250.svg
var creativeAutoSVG []byte

//go:embed themes/finance-300x250.svg
var creativeFinanceSVG []byte

//go:embed themes/gaming-300x250.svg
var creativeGamingSVG []byte

//go:embed themes/privacy-300x250.svg
var creativePrivacySVG []byte

// themeSlugForDomain reduces a creative_domain to a theme slug from the
// svgThemes catalog. Same substring matching cmd/gateway/landing.go uses
// so a creative themed retail lands on a retail-themed landing page.
func themeSlugForDomain(domain string) string {
	d := domain
	switch {
	case containsAny(d, "shoes", "store", "shop", "bite", "retail"):
		return "retail"
	case containsAny(d, "tech", "saas", "cloud", "soft", "crm", "init"):
		return "tech"
	case containsAny(d, "auto", "motors", "drive"):
		return "auto"
	case containsAny(d, "crypto", "finance", "bank", "trade", "invest"):
		return "finance"
	case containsAny(d, "game", "quest", "play"):
		return "gaming"
	case containsAny(d, "vpn", "secure", "privacy", "shield"):
		return "privacy"
	default:
		return "tech"
	}
}

// creativeAssetByTheme returns the object-store key for a creative at
// the requested size. Body bytes are no longer returned — every size
// other than 300x250 is generated on the fly in uploadCreativeAssets,
// so callers just need the key to build a URL and the actual SVG body
// already lives in Minio.
func creativeAssetByTheme(domain string, w, h int) string {
	if w == 0 || h == 0 {
		w, h = 300, 250
	}
	return fmt.Sprintf("themes/%s-%dx%d.svg", themeSlugForDomain(domain), w, h)
}

// useAssetForSize decides whether a creative at this size should embed
// a Minio asset_url image or use inline html_content. The handcrafted
// 300x250 SVGs in themes/ have the most polished look so we keep them
// on the html_content path; every other size renders the procedurally
// generated SVG referenced by an asset URL the gateway proxies. Means
// the size variants are visually distinct from the MPU even when they
// share a theme, which matches real-world creative behaviour where
// agencies hand-tune MPU + programmatically scale the rest.
func useAssetForSize(w, h int) bool {
	if w == 300 && h == 250 {
		return false
	}
	return true
}

// useAssetURL is the legacy ID-parity check; kept temporarily for
// backwards-compat with callers that haven't been migrated to
// useAssetForSize. Even-suffix ID → HTML; odd-suffix ID → asset URL.
func useAssetURL(creativeID string) bool {
	if creativeID == "" {
		return false
	}
	last := creativeID[len(creativeID)-1]
	return last == '1' || last == '3' || last == '5' || last == '7' || last == '9'
}

// uploadCreativeAssets writes every (theme × size) themed SVG into the
// Minio bucket so the asset_url path resolves. Idempotent — Put
// overwrites in place. Called once at seed boot before the campaign
// inserts run. nil store = local-filesystem fallback (seed still
// completes; the asset_url just won't resolve from the browser).
//
// The 300x250 variants for each theme use the hand-crafted SVG files
// embedded above so they keep their original polished look. Every
// other size (728x90, 300x600, 320x50, 160x600, 970x250, 336x280) is
// generated programmatically from the theme catalog in svg_generator.go
// — same palette + glyph so generated and hand-crafted variants read
// as the same brand family.
func uploadCreativeAssets(ctx context.Context, store objects.Store, bucket string, log *slog.Logger) error {
	if store == nil {
		log.Warn("object store unavailable, skipping creative asset upload; asset_url paths will 404")
		return nil
	}
	if err := store.EnsureBucket(ctx, bucket); err != nil {
		log.Warn("ensure bucket failed; asset uploads may fail", "bucket", bucket, "error", err)
	}
	// Make the creatives bucket anonymously readable so the gateway's
	// /v1/creatives proxy (which forwards unsigned browser requests) can serve
	// the SVGs. Without this every creative image 403s in the browser. Only the
	// s3 store implements this; the fs fallback serves files directly.
	if pub, ok := store.(interface {
		SetPublicRead(context.Context, string) error
	}); ok {
		if err := pub.SetPublicRead(ctx, bucket); err != nil {
			log.Warn("set creatives bucket public-read failed; images may 403 in browser", "bucket", bucket, "error", err)
		}
	}

	type asset struct {
		key  string
		body []byte
	}
	var uploads []asset

	// Hand-crafted 300x250 variants — preserved as the reference look.
	handcrafted := map[string][]byte{
		"retail":  creativeRetailSVG,
		"tech":    creativeTechSVG,
		"auto":    creativeAutoSVG,
		"finance": creativeFinanceSVG,
		"gaming":  creativeGamingSVG,
		"privacy": creativePrivacySVG,
	}
	for slug, body := range handcrafted {
		uploads = append(uploads, asset{fmt.Sprintf("themes/%s-300x250.svg", slug), body})
	}

	// Generated variants for every other (theme, size).
	for _, t := range svgThemes {
		for _, s := range standardSizes {
			if s.W == 300 && s.H == 250 {
				continue // handled above
			}
			body := generateThemedSVG(t, s.W, s.H)
			uploads = append(uploads, asset{
				fmt.Sprintf("themes/%s-%dx%d.svg", t.Slug, s.W, s.H),
				body,
			})
		}
	}

	for _, u := range uploads {
		if err := store.Put(ctx, bucket, u.key, bytes.NewReader(u.body), int64(len(u.body)), "image/svg+xml"); err != nil {
			log.Warn("creative asset upload failed", "key", u.key, "error", err)
			continue
		}
		log.Debug("creative asset uploaded", "key", u.key, "bytes", len(u.body))
	}
	log.Info("creative assets uploaded", "count", len(uploads), "bucket", bucket)
	return nil
}

// creativeAssetHTML wraps the asset_url path's <img> in a click
// anchor + impression pixel — same shape the html_content path uses
// (click first, then redirect; impression pixel hidden at the end)
// but with the bulk of the visual coming from the SVG file rather
// than inline emoji + gradient. assetURL is the browser-reachable
// URL (gateway proxy), not the in-cluster Minio URL. ${CLICK_URL} is
// the full signed tracker URL with redir=landing baked in by the ad
// server's macro substitution — no concatenation needed.
func creativeAssetHTML(assetURL string) string {
	return `<a href="${CLICK_URL}" target="_blank" rel="noopener" style="text-decoration:none;display:block;width:${WIDTH}px;height:${HEIGHT}px;position:relative;">` +
		`<img src="` + assetURL + `" alt="" style="display:block;width:100%;height:100%;border-radius:4px;" />` +
		`<img src="${IMP_PIXEL}" width="1" height="1" style="position:absolute;left:0;top:0;opacity:0;" alt="" />` +
		`</a>`
}
