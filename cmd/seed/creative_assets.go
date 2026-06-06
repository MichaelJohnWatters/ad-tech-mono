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

// creativeAssetByTheme returns the object-store key + SVG bytes for
// a domain's theme. Themes mirror creative_templates.go so the HTML
// and image halves of the split look like the same brand family.
func creativeAssetByTheme(domain string) (key string, body []byte) {
	d := domain
	switch {
	case containsAny(d, "shoes", "store", "shop", "bite", "retail"):
		return "themes/retail-300x250.svg", creativeRetailSVG
	case containsAny(d, "tech", "saas", "cloud", "soft", "crm", "init"):
		return "themes/tech-300x250.svg", creativeTechSVG
	case containsAny(d, "auto", "motors", "drive"):
		return "themes/auto-300x250.svg", creativeAutoSVG
	case containsAny(d, "crypto", "finance", "bank", "trade", "invest"):
		return "themes/finance-300x250.svg", creativeFinanceSVG
	case containsAny(d, "game", "quest", "play"):
		return "themes/gaming-300x250.svg", creativeGamingSVG
	case containsAny(d, "vpn", "secure", "privacy", "shield"):
		return "themes/privacy-300x250.svg", creativePrivacySVG
	default:
		return "themes/tech-300x250.svg", creativeTechSVG
	}
}

// useAssetURL decides whether a creative should land on the asset_url
// path (vs the html_content path). Even suffix → HTML; odd suffix →
// asset. Deterministic so re-running seed is idempotent and the same
// creative ID always picks the same path.
func useAssetURL(creativeID string) bool {
	if creativeID == "" {
		return false
	}
	last := creativeID[len(creativeID)-1]
	return last == '1' || last == '3' || last == '5' || last == '7' || last == '9'
}

// uploadCreativeAssets writes every themed SVG into the Minio bucket
// so the asset_url path resolves. Idempotent — Put overwrites in
// place. Called once at seed boot before the campaign inserts run.
// nil store = local-filesystem fallback (seed still completes; the
// asset_url just won't resolve from the browser).
func uploadCreativeAssets(ctx context.Context, store objects.Store, bucket string, log *slog.Logger) error {
	if store == nil {
		log.Warn("object store unavailable, skipping creative asset upload; asset_url paths will 404")
		return nil
	}
	if err := store.EnsureBucket(ctx, bucket); err != nil {
		log.Warn("ensure bucket failed; asset uploads may fail", "bucket", bucket, "error", err)
	}
	uploads := []struct {
		key  string
		body []byte
	}{
		{"themes/retail-300x250.svg", creativeRetailSVG},
		{"themes/tech-300x250.svg", creativeTechSVG},
		{"themes/auto-300x250.svg", creativeAutoSVG},
		{"themes/finance-300x250.svg", creativeFinanceSVG},
		{"themes/gaming-300x250.svg", creativeGamingSVG},
		{"themes/privacy-300x250.svg", creativePrivacySVG},
	}
	for _, u := range uploads {
		if err := store.Put(ctx, bucket, u.key, bytes.NewReader(u.body), int64(len(u.body)), "image/svg+xml"); err != nil {
			log.Warn("creative asset upload failed", "key", u.key, "error", err)
			continue
		}
		log.Debug("creative asset uploaded", "key", u.key, "bytes", len(u.body))
	}
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
