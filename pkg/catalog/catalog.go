// Package catalog is the advertiser product catalog (Dynamic Product Ads).
// A product feed rides the unified ingestion path (pkg/ingest, kind=product)
// into the products table; later consumers read it at render time (dynamic
// creative assembly) and for per-SKU suppression/cross-sell.
package catalog

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Availability values (products.availability CHECK mirrors this set).
const (
	AvailabilityInStock      = "in_stock"
	AvailabilityOutOfStock   = "out_of_stock"
	AvailabilityPreorder     = "preorder"
	AvailabilityDiscontinued = "discontinued"
)

// IsValidAvailability reports whether s is one of the availability values.
func IsValidAvailability(s string) bool {
	switch s {
	case AvailabilityInStock, AvailabilityOutOfStock, AvailabilityPreorder, AvailabilityDiscontinued:
		return true
	}
	return false
}

// Product is one catalog row: everything a dynamic creative template needs to
// render a carted SKU. PriceMicros is micro-dollars (platform money convention).
type Product struct {
	SKU          string `json:"sku"`
	Title        string `json:"title"`
	Description  string `json:"description,omitempty"`
	ImageURL     string `json:"image_url,omitempty"`
	PriceMicros  int64  `json:"price_micros"`
	Currency     string `json:"currency"`
	Availability string `json:"availability"`
	ProductURL   string `json:"product_url,omitempty"`
	Category     string `json:"category,omitempty"`
}

// Store is the catalog persistence seam. Writes are tenant-scoped (RLS GUC per
// account); the cross-tenant render-time read (slice 3) will add a platform-
// hatch method when it lands.
type Store interface {
	// UpsertProducts inserts-or-updates the products for the account keyed on
	// (account_id, sku), stamping source + originTrace lineage. Returns the
	// number of rows written.
	UpsertProducts(ctx context.Context, accountID string, products []Product, source, originTrace string) (int, error)
	// ListByAccount returns the account's products, most recently updated first.
	ListByAccount(ctx context.Context, accountID string, limit int) ([]Product, error)
	// CountByAccount returns how many products the account has.
	CountByAccount(ctx context.Context, accountID string) (int, error)
}

// ParsePriceMicros parses a feed price into micro-dollars plus an optional
// trailing ISO currency ("38.99", "38.99 USD", "$38.99"). Returns the currency
// found in the value ("" when none — caller applies its default).
func ParsePriceMicros(s string) (int64, string, error) {
	v := strings.TrimSpace(s)
	currency := ""
	if fields := strings.Fields(v); len(fields) == 2 && len(fields[1]) == 3 {
		v, currency = fields[0], strings.ToUpper(fields[1])
	}
	v = strings.TrimPrefix(v, "$")
	v = strings.ReplaceAll(v, ",", "")
	if v == "" {
		return 0, "", fmt.Errorf("empty price")
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, "", fmt.Errorf("not a number: %q", s)
	}
	if f < 0 {
		return 0, "", fmt.Errorf("negative price: %q", s)
	}
	// Round to whole micros — feed prices have at most cents of precision.
	return int64(f*1e6 + 0.5), currency, nil
}
