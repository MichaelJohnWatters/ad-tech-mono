// Package postgres implements catalog.Store on the products table. Writes run
// in a tenant transaction (RLS GUC set to the product's account) mirroring
// pkg/audience/store/postgres — the ingest worker writes MANY tenants' feeds
// but always under the claimed job's account, never via the platform hatch.
package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/lib/pq"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/catalog"
)

// Store is the Postgres-backed product catalog.
type Store struct{ db *sql.DB }

// New returns a Store backed by db.
func New(db *sql.DB) *Store { return &Store{db: db} }

// withTenant runs fn inside a transaction with the RLS tenant GUC set, so
// writes are admitted for (and bounded to) the given account.
func (s *Store) withTenant(ctx context.Context, accountID string, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, accountID); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("set tenant: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// UpsertProducts inserts-or-updates the account's products keyed on
// (account_id, sku) in one tenant transaction. Returns rows written.
func (s *Store) UpsertProducts(ctx context.Context, accountID string, products []catalog.Product, source, originTrace string) (int, error) {
	if len(products) == 0 {
		return 0, nil
	}
	const q = `
INSERT INTO products (account_id, sku, title, description, image_url, price_micros,
                      currency, availability, product_url, category, source, origin_trace)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (account_id, sku) DO UPDATE SET
    title = EXCLUDED.title, description = EXCLUDED.description,
    image_url = EXCLUDED.image_url, price_micros = EXCLUDED.price_micros,
    currency = EXCLUDED.currency, availability = EXCLUDED.availability,
    product_url = EXCLUDED.product_url, category = EXCLUDED.category,
    source = EXCLUDED.source, origin_trace = EXCLUDED.origin_trace,
    updated_at = now()`
	written := 0
	err := s.withTenant(ctx, accountID, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, q)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, p := range products {
			if _, err := stmt.ExecContext(ctx, accountID, p.SKU, p.Title, p.Description,
				p.ImageURL, p.PriceMicros, p.Currency, p.Availability, p.ProductURL,
				p.Category, source, originTrace); err != nil {
				return fmt.Errorf("upsert sku %q: %w", p.SKU, err)
			}
			written++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return written, nil
}

const productColumns = `sku, title, description, image_url, price_micros,
       currency, availability, product_url, category`

func scanProduct(scan func(dest ...any) error) (catalog.Product, error) {
	var p catalog.Product
	err := scan(&p.SKU, &p.Title, &p.Description, &p.ImageURL, &p.PriceMicros,
		&p.Currency, &p.Availability, &p.ProductURL, &p.Category)
	return p, err
}

// ListByAccount returns the account's products, most recently updated first.
// Explicit tenant filter per convention (RLS is the safety net).
func (s *Store) ListByAccount(ctx context.Context, accountID string, limit int) ([]catalog.Product, error) {
	if limit <= 0 {
		limit = 200
	}
	out := []catalog.Product{}
	err := s.withTenant(ctx, accountID, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
SELECT `+productColumns+` FROM products
WHERE account_id = $1 ORDER BY updated_at DESC, sku LIMIT $2`, accountID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanProduct(rows.Scan)
			if err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ProductsBySKUs returns the account's products for the given SKUs, in the
// SAME order as skus (freshest-viewed first, from RecentSKUs) — a SKU with no
// catalog row is silently dropped. Reads cross-tenant under the platform hatch:
// the render caller (ad server) is a platform service with no tenant session,
// resolving the winning advertiser's catalog. Bounded by len(skus).
func (s *Store) ProductsBySKUs(ctx context.Context, accountID string, skus []string) ([]catalog.Product, error) {
	if len(skus) == 0 {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin products by skus: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.platform_read', 'on', true)`); err != nil {
		return nil, fmt.Errorf("products by skus platform-read: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `
SELECT `+productColumns+` FROM products
WHERE account_id = $1::uuid AND sku = ANY($2::text[])`, accountID, pq.StringArray(skus))
	if err != nil {
		return nil, fmt.Errorf("products by skus: %w", err)
	}
	defer rows.Close()
	bySKU := map[string]catalog.Product{}
	for rows.Next() {
		p, err := scanProduct(rows.Scan)
		if err != nil {
			return nil, err
		}
		bySKU[p.SKU] = p
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Preserve the caller's SKU order (recency); drop unknown SKUs.
	out := make([]catalog.Product, 0, len(skus))
	for _, sku := range skus {
		if p, ok := bySKU[sku]; ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// ComplementSKUs returns the cross-sell complements for the given SKUs — the
// products.complement_sku of each, de-duplicated, excluding the bought SKUs
// themselves and any empty complement. Used on purchase to rotate the dynamic
// creative toward complementary items (DPA slice 4). Platform-hatch read (the
// consumer service resolves the winning advertiser's catalog).
func (s *Store) ComplementSKUs(ctx context.Context, accountID string, skus []string) ([]string, error) {
	if len(skus) == 0 {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin complement skus: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.platform_read', 'on', true)`); err != nil {
		return nil, fmt.Errorf("complement skus platform-read: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `
SELECT DISTINCT complement_sku FROM products
WHERE account_id = $1::uuid AND sku = ANY($2::text[])
  AND complement_sku <> '' AND NOT (complement_sku = ANY($2::text[]))`,
		accountID, pq.StringArray(skus))
	if err != nil {
		return nil, fmt.Errorf("complement skus: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sku string
		if err := rows.Scan(&sku); err != nil {
			return nil, err
		}
		out = append(out, sku)
	}
	return out, rows.Err()
}

// CountByAccount returns how many products the account has.
func (s *Store) CountByAccount(ctx context.Context, accountID string) (int, error) {
	n := 0
	err := s.withTenant(ctx, accountID, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM products WHERE account_id = $1`, accountID).Scan(&n)
	})
	return n, err
}
