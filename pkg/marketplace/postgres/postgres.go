// Package postgres implements marketplace.Store on the marketplace_listings
// table. Owner writes/reads run in a tenant transaction (RLS GUC = the owner
// account); the cross-tenant catalog browse uses the platform_read hatch.
package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/marketplace"
)

// Store is the Postgres-backed marketplace store.
type Store struct{ db *sql.DB }

// New returns a Store backed by db.
func New(db *sql.DB) *Store { return &Store{db: db} }

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

// UpsertListing creates-or-updates the account's listing for a segment.
func (s *Store) UpsertListing(ctx context.Context, l marketplace.Listing) (string, error) {
	preview := l.Preview
	if len(preview) == 0 {
		preview = []byte("{}")
	}
	status := l.Status
	if status == "" {
		status = marketplace.StatusActive
	}
	var id string
	err := s.withTenant(ctx, l.AccountID, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
INSERT INTO marketplace_listings (account_id, segment_id, name, description, size_estimate, cpm_surcharge_micros, preview, status)
VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7::jsonb, $8)
ON CONFLICT (segment_id) DO UPDATE SET
    name = EXCLUDED.name, description = EXCLUDED.description,
    size_estimate = EXCLUDED.size_estimate, cpm_surcharge_micros = EXCLUDED.cpm_surcharge_micros,
    preview = EXCLUDED.preview, status = EXCLUDED.status, updated_at = now()
RETURNING id::text`,
			l.AccountID, l.SegmentID, l.Name, l.Description, l.SizeEstimate,
			l.CPMSurchargeMicros, string(preview), status).Scan(&id)
	})
	if err != nil {
		return "", fmt.Errorf("upsert listing: %w", err)
	}
	return id, nil
}

const listingCols = `id::text, account_id::text, segment_id::text, name, COALESCE(description,''),
       COALESCE(size_estimate,0), COALESCE(cpm_surcharge_micros,0), preview::text, status,
       created_at, updated_at`

func scanListing(scan func(dest ...any) error) (marketplace.Listing, error) {
	var l marketplace.Listing
	var preview string
	if err := scan(&l.ID, &l.AccountID, &l.SegmentID, &l.Name, &l.Description,
		&l.SizeEstimate, &l.CPMSurchargeMicros, &preview, &l.Status,
		&l.CreatedAt, &l.UpdatedAt); err != nil {
		return marketplace.Listing{}, err
	}
	l.Preview = []byte(preview)
	return l, nil
}

// ListByAccount returns the account's own listings (seller view).
func (s *Store) ListByAccount(ctx context.Context, accountID string, limit int) ([]marketplace.Listing, error) {
	if limit <= 0 {
		limit = 100
	}
	out := []marketplace.Listing{}
	err := s.withTenant(ctx, accountID, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
SELECT `+listingCols+` FROM marketplace_listings
WHERE account_id = $1::uuid ORDER BY created_at DESC LIMIT $2`, accountID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			l, err := scanListing(rows.Scan)
			if err != nil {
				return err
			}
			out = append(out, l)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Catalog returns ACTIVE listings across all tenants (buyer browse), excluding
// the caller's own, joined to the seller's display name. Platform-hatch read.
func (s *Store) Catalog(ctx context.Context, excludeAccountID string, limit int) ([]marketplace.Listing, error) {
	if limit <= 0 {
		limit = 200
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin catalog: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.platform_read', 'on', true)`); err != nil {
		return nil, fmt.Errorf("catalog platform-read: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `
SELECT m.id::text, m.account_id::text, m.segment_id::text, m.name, COALESCE(m.description,''),
       COALESCE(m.size_estimate,0), COALESCE(m.cpm_surcharge_micros,0), m.preview::text, m.status,
       m.created_at, m.updated_at, COALESCE(a.name, '')
FROM marketplace_listings m
JOIN accounts a ON a.id = m.account_id
WHERE m.status = 'active' AND ($1 = '' OR m.account_id <> $1::uuid)
ORDER BY m.created_at DESC LIMIT $2`, excludeAccountID, limit)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	defer rows.Close()
	out := []marketplace.Listing{}
	for rows.Next() {
		var l marketplace.Listing
		var preview, seller string
		if err := rows.Scan(&l.ID, &l.AccountID, &l.SegmentID, &l.Name, &l.Description,
			&l.SizeEstimate, &l.CPMSurchargeMicros, &preview, &l.Status,
			&l.CreatedAt, &l.UpdatedAt, &seller); err != nil {
			return nil, err
		}
		l.Preview = []byte(preview)
		l.SellerName = seller
		out = append(out, l)
	}
	return out, rows.Err()
}

// GetByID returns one active listing by id (cross-tenant). nil when absent/inactive.
func (s *Store) GetByID(ctx context.Context, id string) (*marketplace.Listing, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin get listing: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.platform_read', 'on', true)`); err != nil {
		return nil, fmt.Errorf("get listing platform-read: %w", err)
	}
	l, err := scanListing(tx.QueryRowContext(ctx, `
SELECT `+listingCols+` FROM marketplace_listings
WHERE id = $1::uuid AND status = 'active'`, id).Scan)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get listing: %w", err)
	}
	return &l, nil
}

// SetStatus updates the account's own listing status.
func (s *Store) SetStatus(ctx context.Context, accountID, id, status string) error {
	return s.withTenant(ctx, accountID, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
UPDATE marketplace_listings SET status = $3, updated_at = now()
WHERE id = $1::uuid AND account_id = $2::uuid`, id, accountID, status); err != nil {
			return fmt.Errorf("set listing status: %w", err)
		}
		return nil
	})
}
