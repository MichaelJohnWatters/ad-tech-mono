//go:build e2e

// Dynamic Product Ads slice 3 — the dynamic_product creative assembled at
// render time. The ad server reads the advertiser's catalog + the user's
// carted SKUs (recorded by the slice-2 pixel) and renders the template with
// those products; a user with no SKU context gets the template's static
// {{else}} fallback. This is what makes the chase ad show THE actual carted
// product.
package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestDynamicProductCreativeAssembly(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "dpa-creative")
	uniq := time.Now().UnixNano()
	skuA := fmt.Sprintf("SKU-KIBBLE-%d", uniq)
	skuB := fmt.Sprintf("SKU-TREAT-%d", uniq)

	// A catalog for the advertiser (harness DB is the owner role → RLS bypass).
	if _, err := h.DB.Exec(`
INSERT INTO products (account_id, sku, title, price_micros, currency, availability, product_url, image_url, source)
VALUES ($1::uuid, $2, 'Grain-Free Kibble 12kg', 38990000, 'USD', 'in_stock', 'https://shop/kibble', 'https://img/kibble.png', 'seed'),
       ($1::uuid, $3, 'Training Treats Box', 8490000, 'USD', 'in_stock', 'https://shop/treats', 'https://img/treats.png', 'seed')`,
		w.AdvAcc.ID, skuA, skuB); err != nil {
		t.Fatalf("seed catalog: %v", err)
	}

	// A dynamic_product creative: the template renders carted products, with a
	// static {{else}} branch for no-SKU visitors. ${CLICK_URL} survives to be
	// macro-substituted by the ad server after assembly.
	const tmpl = `<div class="dpa">{{range .Products}}<a href="${CLICK_URL}" data-sku="{{.SKU}}">{{.Title}} {{.PriceDisplay}}</a>{{else}}<a href="${CLICK_URL}">Shop our range</a>{{end}}</div>`
	var creativeID string
	if err := h.DB.QueryRow(`
INSERT INTO creatives (account_id, name, format, width, height, html_content, landing_url, review_status)
VALUES ($1::uuid, 'DPA Creative', 'dynamic_product', 300, 250, $2, 'https://shop', 'approved')
RETURNING id::text`, w.AdvAcc.ID, tmpl).Scan(&creativeID); err != nil {
		t.Fatalf("create dynamic creative: %v", err)
	}
	// The ad server serves creatives from a warm cache — refresh so it sees the
	// new row without waiting for the poll tick.
	h.RefreshAllCaches(t)

	// Record the user's carted SKUs via the slice-2 pixel.
	visitor := fmt.Sprintf("dpa-cr-user-%d", uniq)
	h.FireRetargetingPixelSKUs(t, w.AdvAcc.ID, visitor, "checkout", skuA+","+skuB)
	deadline := time.Now().Add(30 * time.Second)
	for len(h.ProductViewSKUs(t, w.AdvAcc.ID, visitor)) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for product views: %v", h.ProductViewSKUs(t, w.AdvAcc.ID, visitor))
		}
		time.Sleep(500 * time.Millisecond)
	}

	req := models.ServeRequest{
		TraceID:         fmt.Sprintf("dpa-serve-%d", uniq),
		CreativeID:      creativeID,
		AdvertiserID:    w.AdvAcc.ID,
		BehaviourUserID: visitor,
		Width:           300, Height: 250, Currency: "USD", ClearingPrice: 5.0,
	}
	html := h.ServeAdHTML(t, req)
	if !strings.Contains(html, "Grain-Free Kibble 12kg $38.99") {
		t.Fatalf("dynamic creative did not render the carted kibble product: %s", html)
	}
	if !strings.Contains(html, "Training Treats Box $8.49") {
		t.Fatalf("dynamic creative did not render the carted treats product: %s", html)
	}
	if strings.Contains(html, "Shop our range") {
		t.Errorf("static fallback rendered despite the user having carted products: %s", html)
	}
	// The click macro was substituted into a real signed tracker URL.
	if !strings.Contains(html, "/v1/t/click?") || strings.Contains(html, "${CLICK_URL}") {
		t.Errorf("click macro not substituted in assembled HTML: %s", html)
	}

	// A fresh user with no SKU context → the static {{else}} branch.
	fresh := models.ServeRequest{
		TraceID:         fmt.Sprintf("dpa-fresh-%d", uniq),
		CreativeID:      creativeID,
		AdvertiserID:    w.AdvAcc.ID,
		BehaviourUserID: fmt.Sprintf("dpa-never-seen-%d", uniq),
		Width:           300, Height: 250, Currency: "USD", ClearingPrice: 5.0,
	}
	freshHTML := h.ServeAdHTML(t, fresh)
	if !strings.Contains(freshHTML, "Shop our range") {
		t.Fatalf("fresh user did not get the static fallback: %s", freshHTML)
	}
	if strings.Contains(freshHTML, "Grain-Free Kibble") {
		t.Errorf("fresh user got carted products (leak): %s", freshHTML)
	}
}
