package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/catalog"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
)

type fakeSKUs struct {
	skus []string
	err  error
}

func (f fakeSKUs) RecentSKUs(_ context.Context, _, _ string, _ int) ([]string, error) {
	return f.skus, f.err
}

// fakeSKUsByKey returns SKUs keyed by the lookup key (user id or household),
// so a test can give only the household carted products.
type fakeSKUsByKey struct{ m map[string][]string }

func (f fakeSKUsByKey) RecentSKUs(_ context.Context, _, key string, _ int) ([]string, error) {
	return f.m[key], nil
}

type fakeProducts struct {
	products []catalog.Product
	err      error
}

func (f fakeProducts) ProductsBySKUs(_ context.Context, _ string, _ []string) ([]catalog.Product, error) {
	return f.products, f.err
}

func dpLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

const dpTemplate = `<div>{{range .Products}}<a href="{{.ProductURL}}" class="p">{{.Title}} {{.PriceDisplay}}</a>{{else}}<a href="${LandingURL}">Shop now</a>{{end}}</div>`

func dpCreative() AdCreative {
	return AdCreative{ID: "cr1", Format: constants.FormatDynamicProduct, HTML: dpTemplate, LandingURL: "https://shop/x"}
}

// With SKU context, the template renders the carted products (recency order).
func TestDynamicProduct_RendersCartedProducts(t *testing.T) {
	r := &dynamicProductRenderer{
		skus: fakeSKUs{skus: []string{"SKU-A", "SKU-B"}},
		products: fakeProducts{products: []catalog.Product{
			{SKU: "SKU-A", Title: "Kibble", PriceMicros: 38_990_000, Currency: "USD", ProductURL: "https://shop/a"},
			{SKU: "SKU-B", Title: "Treats", PriceMicros: 8_490_000, Currency: "USD", ProductURL: "https://shop/b"},
		}},
		maxItems: 6, log: dpLog(),
	}
	out := r.Assemble(context.Background(), dpCreative(), "adv1", "user1", "")
	if !strings.Contains(out, "Kibble $38.99") || !strings.Contains(out, "Treats $8.49") {
		t.Fatalf("assembled HTML missing products: %s", out)
	}
	if strings.Contains(out, "Shop now") {
		t.Errorf("static fallback rendered despite products present: %s", out)
	}
	// Order preserved (A before B).
	if strings.Index(out, "Kibble") > strings.Index(out, "Treats") {
		t.Errorf("product order not preserved: %s", out)
	}
}

// No SKU context (fresh visitor) renders the template's static {{else}} branch,
// with the ${LandingURL} macro left for downstream substitution.
func TestDynamicProduct_StaticFallbackNoSKUs(t *testing.T) {
	r := &dynamicProductRenderer{
		skus: fakeSKUs{skus: nil}, products: fakeProducts{}, maxItems: 6, log: dpLog(),
	}
	out := r.Assemble(context.Background(), dpCreative(), "adv1", "user1", "")
	if !strings.Contains(out, "Shop now") || !strings.Contains(out, "${LandingURL}") {
		t.Fatalf("expected static fallback with macro intact, got: %s", out)
	}
}

// An empty user id (no personalisation consent) never looks up SKUs → static.
func TestDynamicProduct_NoUserStaticFallback(t *testing.T) {
	r := &dynamicProductRenderer{
		skus:     fakeSKUs{skus: []string{"SKU-A"}}, // would return SKUs, but must not be consulted
		products: fakeProducts{products: []catalog.Product{{SKU: "SKU-A", Title: "Kibble"}}},
		maxItems: 6, log: dpLog(),
	}
	out := r.Assemble(context.Background(), dpCreative(), "adv1", "", "")
	if !strings.Contains(out, "Shop now") {
		t.Fatalf("empty user should render static fallback, got: %s", out)
	}
}

// Cross-site chase: the user id has no SKUs but the HOUSEHOLD does — the
// creative renders the household's carted products (the shared key the DSP
// matched to win the auction).
func TestDynamicProduct_HouseholdFallbackRendersProducts(t *testing.T) {
	r := &dynamicProductRenderer{
		skus: fakeSKUsByKey{m: map[string][]string{"hh:home1": {"SKU-A"}}}, // only the household has views
		products: fakeProducts{products: []catalog.Product{
			{SKU: "SKU-A", Title: "Kibble", PriceMicros: 38_990_000, Currency: "USD", ProductURL: "https://shop/a"},
		}},
		maxItems: 6, log: dpLog(),
	}
	// Publisher-side user id (no views) + the household (has the carted SKU).
	out := r.Assemble(context.Background(), dpCreative(), "adv1", "pub-user-xyz", "hh:home1")
	if !strings.Contains(out, "Kibble $38.99") {
		t.Fatalf("household fallback did not render the carted product cross-site: %s", out)
	}
	if strings.Contains(out, "Shop now") {
		t.Errorf("static fallback rendered despite the household having products: %s", out)
	}
}

// A store error falls back to static, never fails the serve.
func TestDynamicProduct_StoreErrorFallsBack(t *testing.T) {
	r := &dynamicProductRenderer{
		skus: fakeSKUs{err: errors.New("db down")}, products: fakeProducts{}, maxItems: 6, log: dpLog(),
	}
	out := r.Assemble(context.Background(), dpCreative(), "adv1", "user1", "")
	if !strings.Contains(out, "Shop now") {
		t.Fatalf("store error should render static fallback, got: %s", out)
	}
}

// A nil renderer (stores unavailable at boot) still renders the static branch.
func TestDynamicProduct_NilRendererStatic(t *testing.T) {
	var r *dynamicProductRenderer
	out := r.Assemble(context.Background(), dpCreative(), "adv1", "user1", "")
	if !strings.Contains(out, "Shop now") {
		t.Fatalf("nil renderer should render static fallback, got: %s", out)
	}
}

// A non-dynamic creative passes through untouched.
func TestDynamicProduct_NonDynamicUntouched(t *testing.T) {
	r := &dynamicProductRenderer{skus: fakeSKUs{}, products: fakeProducts{}, log: dpLog()}
	c := AdCreative{ID: "cr2", Format: constants.FormatBanner, HTML: "<b>static banner</b>"}
	if out := r.Assemble(context.Background(), c, "adv1", "user1", ""); out != "<b>static banner</b>" {
		t.Fatalf("non-dynamic creative modified: %s", out)
	}
}
