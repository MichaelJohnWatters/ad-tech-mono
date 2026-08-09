//go:build e2e

// Dynamic Product Ads slice 1 — the product catalog feed rides the unified
// ingestion path (kind=product): a CSV upload lands in the products table,
// re-uploads upsert by SKU, and a wrong-shaped feed is rejected whole (422,
// nothing imported). The counterpart to the audience upload e2e.
package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestProductCatalogFeedIngest(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	adv := h.CreateAdvertiser(t, "prod-catalog-adv")

	// A well-formed feed with canonical + Google-Merchant-style headers.
	feed := strings.Join([]string{
		"sku,title,price,image_link,availability,category,link",
		"SKU-1,Widget One,19.99,https://shop/img/1.png,in stock,widgets,https://shop/p/1",
		"SKU-2,Widget Two,129.00 EUR,https://shop/img/2.png,in_stock,widgets,https://shop/p/2",
		"SKU-3,Gizmo,$5.50,,preorder,gizmos,",
	}, "\n")
	status, body := h.UploadProductFeed(t, adv.ID, "shop-catalog", feed)
	if status != 200 {
		t.Fatalf("feed upload status %d: %s", status, body)
	}
	if !strings.Contains(body, `"products_written":3`) {
		t.Fatalf("expected 3 products written, got: %s", body)
	}

	products := h.ListProducts(t, adv.ID)
	if len(products) != 3 {
		t.Fatalf("catalog has %d products, want 3", len(products))
	}
	bySKU := map[string]int64{}
	for _, p := range products {
		bySKU[p.SKU] = p.PriceMicros
	}
	if bySKU["SKU-1"] != 19_990_000 {
		t.Errorf("SKU-1 price = %d micros, want 19_990_000", bySKU["SKU-1"])
	}
	if bySKU["SKU-2"] != 129_000_000 {
		t.Errorf("SKU-2 price = %d micros, want 129_000_000", bySKU["SKU-2"])
	}

	// Re-upload upserts by SKU (idempotent count, updated price) + adds one new.
	feed2 := strings.Join([]string{
		"sku,title,price",
		"SKU-1,Widget One Deluxe,24.99",
		"SKU-4,Widget Four,9.00",
	}, "\n")
	status, body = h.UploadProductFeed(t, adv.ID, "shop-catalog", feed2)
	if status != 200 {
		t.Fatalf("re-upload status %d: %s", status, body)
	}
	products = h.ListProducts(t, adv.ID)
	if len(products) != 4 {
		t.Fatalf("after re-upload catalog has %d products, want 4 (SKU-1 upserted, SKU-4 new)", len(products))
	}
	for _, p := range products {
		if p.SKU == "SKU-1" {
			if p.PriceMicros != 24_990_000 {
				t.Errorf("SKU-1 after re-upload = %d micros, want 24_990_000 (upsert)", p.PriceMicros)
			}
			if p.Title != "Widget One Deluxe" {
				t.Errorf("SKU-1 title not upserted: %q", p.Title)
			}
		}
	}

	// A feed with a bad row is rejected WHOLE (strict all-or-nothing): 422,
	// nothing imported, and the catalog is unchanged.
	bad := strings.Join([]string{
		"sku,title,price",
		"SKU-5,Valid Row,10.00",
		"SKU-6,Missing Price",
	}, "\n")
	status, body = h.UploadProductFeed(t, adv.ID, "shop-catalog", bad)
	if status != 422 {
		t.Fatalf("bad feed status %d (want 422): %s", status, body)
	}
	if got := len(h.ListProducts(t, adv.ID)); got != 4 {
		t.Fatalf("bad feed changed the catalog: %d products, want 4 (nothing imported)", got)
	}
}
