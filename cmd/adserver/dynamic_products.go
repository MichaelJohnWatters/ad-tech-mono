package main

// dynamic_products.go — Dynamic Product Ads slice 3: assemble a creative at
// render time from the advertiser's product catalog + the user's recently
// viewed/carted SKUs (retargeting_product_views, slice 2).
//
// A creative with format=dynamic_product carries a Go template in html_content.
// At serve time we read the user's recent SKUs, look them up in the catalog,
// and execute the template with those products. The template's {{else}} branch
// is the STATIC fallback rendered when the user has no SKU context (a fresh
// visitor, an unknown SKU, or the stores being unavailable). After assembly the
// normal macro substitution runs on the result, so ${CLICK_URL}/${LandingURL}
// etc. still work in either branch.
//
// This is a post-auction render read (once per won impression), not the bid
// loop, so the two bounded indexed queries are allowed — the same shape as the
// existing per-serve Minio body fetch. Any failure falls back to the template's
// empty-products render, so a dynamic creative never renders worse than static.

import (
	"bytes"
	"context"
	"html/template"
	"log/slog"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/catalog"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
)

// skuReader returns a user's recently-viewed SKUs for an advertiser (freshest
// first). Implemented by pkg/audience/store/postgres.Store.RecentSKUs.
type skuReader interface {
	RecentSKUs(ctx context.Context, accountID, userID string, limit int) ([]string, error)
}

// productReader resolves catalog products for a set of SKUs, order-preserving.
// Implemented by pkg/catalog/postgres.Store.ProductsBySKUs.
type productReader interface {
	ProductsBySKUs(ctx context.Context, accountID string, skus []string) ([]catalog.Product, error)
}

// dynamicProductRenderer assembles dynamic_product creatives. nil-safe: a nil
// renderer (stores unavailable at boot) leaves every creative untouched, so the
// dynamic template renders its {{else}} static branch.
type dynamicProductRenderer struct {
	skus     skuReader
	products productReader
	maxItems int
	timeout  time.Duration
	log      *slog.Logger
}

// dpTemplateData is what a dynamic_product template executes against.
type dpTemplateData struct {
	Products []catalog.Product
	UserID   string
}

// Assemble returns the creative's render-ready HTML. For a dynamic_product
// creative it executes the html_content template with the user's carted
// products; for any other format (or a nil renderer / parse error) it returns
// the creative's HTML unchanged. Macro substitution runs on the result upstream.
func (d *dynamicProductRenderer) Assemble(ctx context.Context, creative AdCreative, advertiserID, userID string) string {
	if creative.Format != constants.FormatDynamicProduct {
		return creative.HTML
	}
	// Parse the template up front — a malformed template can't render, so fall
	// back to the raw html_content (creative authors validate at creation).
	tmpl, err := template.New("dpa").Parse(creative.HTML)
	if err != nil {
		if d != nil && d.log != nil {
			d.log.Warn("dynamic product: template parse failed, serving raw html", "creative", creative.ID, "error", err)
		}
		return creative.HTML
	}

	var products []catalog.Product
	// Only attempt a data-driven render when we have the stores AND a user to
	// personalise for. Otherwise fall straight through to the {{else}} branch.
	if d != nil && d.skus != nil && d.products != nil && advertiserID != "" && userID != "" {
		products = d.lookup(ctx, advertiserID, userID)
	}
	return d.execute(tmpl, creative, dpTemplateData{Products: products, UserID: userID})
}

// lookup reads the user's recent SKUs then resolves them against the catalog,
// under a bounded timeout. Any error → no products (the template's static
// branch), logged but never fatal to the serve.
func (d *dynamicProductRenderer) lookup(ctx context.Context, advertiserID, userID string) []catalog.Product {
	lctx := ctx
	if d.timeout > 0 {
		var cancel context.CancelFunc
		lctx, cancel = context.WithTimeout(ctx, d.timeout)
		defer cancel()
	}
	skus, err := d.skus.RecentSKUs(lctx, advertiserID, userID, d.maxItems)
	if err != nil {
		d.log.Warn("dynamic product: recent skus read failed, static fallback", "advertiser", advertiserID, "error", err)
		return nil
	}
	if len(skus) == 0 {
		return nil
	}
	products, err := d.products.ProductsBySKUs(lctx, advertiserID, skus)
	if err != nil {
		d.log.Warn("dynamic product: catalog lookup failed, static fallback", "advertiser", advertiserID, "error", err)
		return nil
	}
	return products
}

// execute runs the parsed template; a runtime execution error falls back to the
// creative's raw HTML rather than emitting a half-rendered page.
func (d *dynamicProductRenderer) execute(tmpl *template.Template, creative AdCreative, data dpTemplateData) string {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		if d != nil && d.log != nil {
			d.log.Warn("dynamic product: template execute failed, serving raw html", "creative", creative.ID, "error", err)
		}
		return creative.HTML
	}
	return buf.String()
}
