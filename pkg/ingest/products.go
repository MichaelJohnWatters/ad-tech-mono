package ingest

// products.go — the PRODUCT feed branch of the unified ingestion path (Dynamic
// Product Ads slice 1). A product catalog CSV rides the exact same machinery as
// an audience file — staged object, one audience_ingest_jobs row
// (kind=product), same lease/retry/quarantine/strict-all-or-nothing semantics —
// but lands in the products table instead of segment memberships. No segment,
// no match rate, no profile.signal publish.

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/catalog"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ingestjobs"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/pgp"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/pipeline"
)

// CatalogWriter is the product-persistence seam (implemented by
// pkg/catalog/postgres.Store). nil on a Processor = product jobs fail loudly
// ("product ingest not configured") rather than silently no-oping.
type CatalogWriter interface {
	UpsertProducts(ctx context.Context, accountID string, products []catalog.Product, source, originTrace string) (int, error)
}

// productColumnAliases normalize common feed headers (including Google
// Merchant-style names) to canonical product fields. Headers are lowercased at
// decode.
var productColumnAliases = map[string]string{
	"sku": "sku", "id": "sku", "item_id": "sku", "offer_id": "sku",
	"title": "title", "name": "title", "product_name": "title",
	"description": "description",
	"price":       "price",
	"image_url":   "image_url", "image_link": "image_url", "image": "image_url",
	"product_url": "product_url", "link": "product_url", "url": "product_url",
	"availability": "availability", "stock_status": "availability",
	"category": "category", "product_category": "category", "google_product_category": "category",
	"currency": "currency",
}

// ParseProducts turns decoded feed rows into catalog.Products, quarantining
// rows that fail validation (missing sku/title, unparseable price, unknown
// availability). Pure — unit-testable without I/O. defaultCurrency applies to
// rows whose price carries no currency suffix ("" → USD).
func ParseProducts(records []pipeline.Record, defaultCurrency string) ([]catalog.Product, []pipeline.QuarantineRecord) {
	if defaultCurrency == "" {
		defaultCurrency = "USD"
	}
	valid := make([]catalog.Product, 0, len(records))
	var quarantine []pipeline.QuarantineRecord
	for _, rec := range records {
		// Fold aliased columns into canonical fields (first non-empty wins).
		fields := map[string]string{}
		for col, v := range rec {
			canon, ok := productColumnAliases[strings.ToLower(col)]
			if !ok || strings.TrimSpace(v) == "" {
				continue
			}
			if _, exists := fields[canon]; !exists {
				fields[canon] = strings.TrimSpace(v)
			}
		}
		var errs []pipeline.ValidationError
		fail := func(field, value, msg string) {
			errs = append(errs, pipeline.ValidationError{Field: field, Value: value, Rule: "product", Message: msg})
		}
		if fields["sku"] == "" {
			fail("sku", "", "sku is required (columns sku/id/item_id/offer_id)")
		}
		if fields["title"] == "" {
			fail("title", "", "title is required (columns title/name/product_name)")
		}
		priceMicros := int64(0)
		currency := ""
		if fields["price"] == "" {
			fail("price", "", "price is required")
		} else {
			var perr error
			priceMicros, currency, perr = catalog.ParsePriceMicros(fields["price"])
			if perr != nil {
				fail("price", fields["price"], perr.Error())
			}
		}
		if c := fields["currency"]; c != "" {
			currency = strings.ToUpper(c)
		}
		if currency == "" {
			currency = defaultCurrency
		}
		availability := strings.ReplaceAll(strings.ToLower(fields["availability"]), " ", "_")
		if availability == "" {
			availability = catalog.AvailabilityInStock
		}
		if !catalog.IsValidAvailability(availability) {
			fail("availability", fields["availability"], "must be one of in_stock, out_of_stock, preorder, discontinued")
		}
		if len(errs) > 0 {
			quarantine = append(quarantine, pipeline.QuarantineRecord{Record: rec, Errors: errs})
			continue
		}
		valid = append(valid, catalog.Product{
			SKU:          fields["sku"],
			Title:        fields["title"],
			Description:  fields["description"],
			ImageURL:     fields["image_url"],
			PriceMicros:  priceMicros,
			Currency:     currency,
			Availability: availability,
			ProductURL:   fields["product_url"],
			Category:     fields["category"],
		})
	}
	return valid, quarantine
}

// ProcessProducts is the kind=product twin of Process: staged file → decrypt →
// decode → ParseProducts → strict threshold → UpsertProducts → move to
// processed/. Same infra-vs-content error contract; IngestResult.MembersAdded
// carries products written.
func (p *Processor) ProcessProducts(ctx context.Context, job ingestjobs.Job) (ingestjobs.IngestResult, error) {
	if p.Catalog == nil {
		return ingestjobs.IngestResult{}, fmt.Errorf("product ingest not configured (no catalog store)")
	}
	started := time.Now().UTC()
	provider, key, bucket := job.Provider, job.FileKey, job.FileBucket
	log := p.Log.With("trace_id", job.Trace(), "provider", provider, "file", key, "job", job.ID, "kind", ingestjobs.KindProduct)

	body, err := p.readObject(ctx, bucket, key)
	if err != nil {
		log.Error("product ingest: read file failed (will retry)", "error", err)
		return ingestjobs.IngestResult{}, infraErr{err}
	}
	body, wasEncrypted, derr := pgp.MaybeDecrypt(body, p.PGPKeyring)
	if derr != nil {
		log.Error("product ingest: pgp decrypt failed", "encrypted", wasEncrypted, "error", derr)
		return p.quarantineStaged(ctx, log, bucket, provider, key, job.AccountID, started, pgpRejectReason)
	}
	records, err := DecodeFile(ctx, path.Base(key), body)
	if err != nil {
		return p.quarantineStaged(ctx, log, bucket, provider, key, job.AccountID, started, fmt.Sprintf("decode: %v", err))
	}
	if len(records) == 0 {
		return p.quarantineStaged(ctx, log, bucket, provider, key, job.AccountID, started, "no data rows")
	}

	products, quarantined := ParseProducts(records, "")
	result := pipeline.Result{
		Quarantine: quarantined,
		Stats:      pipeline.Stats{TotalInput: len(records), Valid: len(products), Quarantined: len(quarantined)},
	}
	// Valid rows only matter for counts here — synthesize records for finishFile.
	result.Valid = make([]pipeline.Record, len(products))

	rejectedKey := ""
	if len(quarantined) > 0 {
		rejectedKey = rejectedPrefix(provider) + path.Base(key)
		if err := p.writeRejected(ctx, bucket, rejectedKey, quarantined); err != nil {
			log.Error("product ingest: persist quarantine failed (will retry)", "error", err)
			return ingestjobs.IngestResult{}, infraErr{err}
		}
	}
	if len(products) == 0 {
		if _, err := p.finishFile(ctx, log, bucket, provider, key, job.AccountID, started, "", 0, result, rejectedKey,
			"no valid product rows"); err != nil {
			return ingestjobs.IngestResult{}, err
		}
		return ingestjobs.IngestResult{RejectedRows: len(quarantined), RejectedKey: rejectedKey},
			rejectErr{fmt.Sprintf("no valid product rows: none of the %d rows had sku+title+price after validation%s",
				len(records), firstQuarantineDetail(quarantined))}
	}
	// Atomic per-file, same threshold semantics as audience files.
	total := len(products) + len(quarantined)
	if len(quarantined) > 0 && len(quarantined)*100 > p.MaxRejectPct*total {
		if _, err := p.finishFile(ctx, log, bucket, provider, key, job.AccountID, started, "", 0, result, rejectedKey,
			"rejected rows exceed threshold"); err != nil {
			return ingestjobs.IngestResult{}, err
		}
		return ingestjobs.IngestResult{RejectedRows: len(quarantined), RejectedKey: rejectedKey},
			rejectErr{fmt.Sprintf("%d of %d rows failed validation — file rejected, nothing imported%s (see %s)",
				len(quarantined), total, firstQuarantineDetail(quarantined), rejectedKey)}
	}

	written, err := p.Catalog.UpsertProducts(ctx, job.AccountID, products, signalSource(job), job.Trace())
	if err != nil {
		log.Error("product ingest: upsert products failed (will retry)", "error", err)
		return ingestjobs.IngestResult{}, infraErr{err}
	}

	res, err := p.finishFile(ctx, log, bucket, provider, key, job.AccountID, started, "", 0, result, rejectedKey, "")
	if err != nil {
		return ingestjobs.IngestResult{}, err
	}
	res.MembersAdded = written
	log.Info("product ingest: file processed", "valid", len(products),
		"rejected", len(quarantined), "written", written)
	return res, nil
}

// ValidateProductSample is the product-feed pre-flight for the synchronous
// upload path: decode + sample n rows through the SAME ParseProducts rules, so
// a wrong-shaped feed 422s before staging. Mirrors ValidateSample.
func (p *Processor) ValidateProductSample(ctx context.Context, name string, body []byte, n int) error {
	body, _, derr := pgp.MaybeDecrypt(body, p.PGPKeyring)
	if derr != nil {
		return Reject(pgpRejectReason)
	}
	records, err := DecodeFile(ctx, name, body)
	if err != nil {
		return Reject(fmt.Sprintf("could not parse file: %v", err))
	}
	if len(records) == 0 {
		return Reject("file has no data rows")
	}
	if len(records) > n {
		records = records[:n]
	}
	valid, quarantined := ParseProducts(records, "")
	if len(valid) == 0 {
		return Reject(fmt.Sprintf("no valid product rows in the first %d row(s) — a product feed needs sku (sku/id/item_id), title (title/name) and price columns%s",
			len(records), firstQuarantineDetail(quarantined)))
	}
	total := len(valid) + len(quarantined)
	if len(quarantined) > 0 && len(quarantined)*100 > p.MaxRejectPct*total {
		return Reject(fmt.Sprintf("%d of the first %d row(s) failed validation — file rejected, all-or-nothing%s (raise ingest.max_reject_pct to allow partial imports)",
			len(quarantined), total, firstQuarantineDetail(quarantined)))
	}
	return nil
}
