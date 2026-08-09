//go:build e2e

// Dynamic Product Ads slice 2 — the SKU-aware retargeting pixel. Firing
// /v1/t/rt with skus= records the shopper's viewed/carted products in
// retargeting_product_views (person + household), the memory a dynamic
// creative (slice 3) renders from. Independent of segment matching: a
// product-page pixel builds SKU memory even before enrollment.
package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestProductSKUPixelRecordsViews(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "dpa-sku")
	uniq := time.Now().UnixNano()
	visitor := fmt.Sprintf("dpa-visitor-%d", uniq)

	// No product views before any pixel.
	if got := h.ProductViewSKUs(t, w.AdvAcc.ID, visitor); len(got) != 0 {
		t.Fatalf("visitor already has product views before any pixel: %v", got)
	}

	// Fire the SKU-aware pixel — two SKUs the shopper viewed. audience-rt
	// records them within seconds (same path as enrollment).
	h.FireRetargetingPixelSKUs(t, w.AdvAcc.ID, visitor, "product", "SKU-ALPHA,SKU-BETA")

	deadline := time.Now().Add(30 * time.Second)
	var got []string
	for {
		got = h.ProductViewSKUs(t, w.AdvAcc.ID, visitor)
		if len(got) >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if len(got) != 2 {
		t.Fatalf("recorded SKUs = %v, want 2 (SKU-ALPHA, SKU-BETA)", got)
	}
	seen := map[string]bool{}
	for _, s := range got {
		seen[s] = true
	}
	if !seen["SKU-ALPHA"] || !seen["SKU-BETA"] {
		t.Fatalf("recorded SKUs = %v, want SKU-ALPHA + SKU-BETA", got)
	}

	// A repeat view of one SKU upserts (no duplicate row), and adds a new one.
	h.FireRetargetingPixelSKUs(t, w.AdvAcc.ID, visitor, "product", "SKU-ALPHA,SKU-GAMMA")
	deadline = time.Now().Add(30 * time.Second)
	for {
		got = h.ProductViewSKUs(t, w.AdvAcc.ID, visitor)
		if len(got) >= 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if len(got) != 3 {
		t.Fatalf("after repeat view, SKUs = %v, want exactly 3 (ALPHA upserted, GAMMA new)", got)
	}
}
