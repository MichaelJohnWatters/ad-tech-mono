package ingest

import (
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/catalog"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/pipeline"
)

func TestParseProductsCanonicalAndAliases(t *testing.T) {
	records := []pipeline.Record{
		// Canonical headers.
		{"sku": "DOG-001", "title": "Premium Dog Food 12kg", "price": "38.99",
			"image_url": "https://shop/img/dog.png", "product_url": "https://shop/p/dog",
			"availability": "in_stock", "category": "Pet Supplies", "description": "Grain-free."},
		// Google Merchant-style headers + "in stock" + price with currency.
		{"id": "CAT-002", "name": "Cat Tree Deluxe", "price": "129.00 EUR",
			"image_link": "https://shop/img/cat.png", "link": "https://shop/p/cat",
			"availability": "in stock", "google_product_category": "Pet Supplies"},
		// Minimal: sku+title+price only; defaults applied.
		{"offer_id": "TOY-003", "product_name": "Rope Toy", "price": "$5.50"},
	}
	valid, quarantined := ParseProducts(records, "")
	if len(quarantined) != 0 {
		t.Fatalf("expected 0 quarantined, got %d: %+v", len(quarantined), quarantined)
	}
	if len(valid) != 3 {
		t.Fatalf("expected 3 valid, got %d", len(valid))
	}
	p := valid[0]
	if p.SKU != "DOG-001" || p.Title != "Premium Dog Food 12kg" || p.PriceMicros != 38_990_000 ||
		p.Currency != "USD" || p.Availability != catalog.AvailabilityInStock ||
		p.Category != "Pet Supplies" || p.Description != "Grain-free." {
		t.Fatalf("canonical row parsed wrong: %+v", p)
	}
	g := valid[1]
	if g.SKU != "CAT-002" || g.Title != "Cat Tree Deluxe" || g.PriceMicros != 129_000_000 ||
		g.Currency != "EUR" || g.Availability != catalog.AvailabilityInStock ||
		g.ImageURL != "https://shop/img/cat.png" || g.ProductURL != "https://shop/p/cat" ||
		g.Category != "Pet Supplies" {
		t.Fatalf("google-style row parsed wrong: %+v", g)
	}
	m := valid[2]
	if m.SKU != "TOY-003" || m.PriceMicros != 5_500_000 || m.Currency != "USD" ||
		m.Availability != catalog.AvailabilityInStock {
		t.Fatalf("minimal row parsed wrong: %+v", m)
	}
}

func TestParseProductsQuarantinesBadRows(t *testing.T) {
	records := []pipeline.Record{
		{"sku": "OK-1", "title": "Fine", "price": "10"},
		{"title": "No SKU", "price": "10"},                                        // missing sku
		{"sku": "NO-TITLE", "price": "10"},                                        // missing title
		{"sku": "NO-PRICE", "title": "No price"},                                  // missing price
		{"sku": "BAD-PRICE", "title": "Bad price", "price": "cheap"},              // unparseable price
		{"sku": "BAD-AVAIL", "title": "X", "price": "1", "availability": "maybe"}, // bad enum
	}
	valid, quarantined := ParseProducts(records, "")
	if len(valid) != 1 || valid[0].SKU != "OK-1" {
		t.Fatalf("expected only OK-1 valid, got %+v", valid)
	}
	if len(quarantined) != 5 {
		t.Fatalf("expected 5 quarantined, got %d", len(quarantined))
	}
	for _, q := range quarantined {
		if len(q.Errors) == 0 {
			t.Fatalf("quarantined row carries no error: %+v", q)
		}
	}
}

func TestParsePriceMicros(t *testing.T) {
	cases := []struct {
		in       string
		micros   int64
		currency string
		wantErr  bool
	}{
		{"38.99", 38_990_000, "", false},
		{"38.99 USD", 38_990_000, "USD", false},
		{"38.99 eur", 38_990_000, "EUR", false},
		{"$1,299.00", 1_299_000_000, "", false},
		{"0", 0, "", false},
		{"", 0, "", true},
		{"free", 0, "", true},
		{"-5", 0, "", true},
	}
	for _, c := range cases {
		micros, currency, err := catalog.ParsePriceMicros(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParsePriceMicros(%q): expected error, got %d", c.in, micros)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParsePriceMicros(%q): %v", c.in, err)
			continue
		}
		if micros != c.micros || currency != c.currency {
			t.Errorf("ParsePriceMicros(%q) = (%d, %q), want (%d, %q)", c.in, micros, currency, c.micros, c.currency)
		}
	}
}
