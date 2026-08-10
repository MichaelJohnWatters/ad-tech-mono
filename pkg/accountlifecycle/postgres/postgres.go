// Package postgres implements accountlifecycle.Store. All work runs in a tenant
// transaction (RLS GUC = the account), so line_items / placements / the closure
// row are all scoped to the account; the accounts table has no RLS so its status
// flip is scoped by an explicit WHERE id.
package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/lib/pq"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/accountlifecycle"
)

// Store is the Postgres-backed account-lifecycle store.
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

// RequestClosure opens a grace-period closure and applies the reversible side
// effects (suspend account, pause live line items, deactivate placements).
func (s *Store) RequestClosure(ctx context.Context, accountID, requestedBy, reason string, graceDays int) (accountlifecycle.ClosureRequest, error) {
	if graceDays <= 0 {
		graceDays = accountlifecycle.DefaultGraceDays
	}
	var out accountlifecycle.ClosureRequest
	err := s.withTenant(ctx, accountID, func(tx *sql.Tx) error {
		// One grace closure at a time. Check first so we don't pause anything
		// before discovering a conflict (the partial unique index is the backstop).
		var existing string
		switch err := tx.QueryRowContext(ctx,
			`SELECT id::text FROM account_closure_requests WHERE account_id = $1::uuid AND status = 'grace'`,
			accountID).Scan(&existing); err {
		case nil:
			return accountlifecycle.ErrAlreadyClosing
		case sql.ErrNoRows:
			// no open closure — proceed
		default:
			return fmt.Errorf("check existing closure: %w", err)
		}

		// Pause the account's live campaigns, capturing exactly which ids we
		// touched so cancel can restore only those.
		pausedItems, err := collectIDs(ctx, tx,
			`UPDATE line_items SET status = 'paused', updated_at = now()
			 WHERE account_id = $1::uuid AND status = 'live' RETURNING id::text`, accountID)
		if err != nil {
			return fmt.Errorf("pause line items: %w", err)
		}
		// Deactivate the account's active placements (publishers: no new auctions).
		deactPlacements, err := collectIDs(ctx, tx,
			`UPDATE placements SET status = 'inactive', updated_at = now()
			 WHERE account_id = $1::uuid AND status = 'active' RETURNING id::text`, accountID)
		if err != nil {
			return fmt.Errorf("deactivate placements: %w", err)
		}

		graceEnds := time.Now().Add(time.Duration(graceDays) * 24 * time.Hour)
		var requestedByArg any
		if requestedBy != "" {
			requestedByArg = requestedBy
		}
		row := tx.QueryRowContext(ctx, `
INSERT INTO account_closure_requests
  (account_id, status, reason, requested_by, grace_ends_at, paused_line_items, deactivated_placements)
VALUES ($1::uuid, 'grace', $2, $3, $4, $5, $6)
RETURNING `+closureCols,
			accountID, reason, requestedByArg, graceEnds,
			pq.Array(pausedItems), pq.Array(deactPlacements))
		var err2 error
		out, err2 = scanClosure(row.Scan)
		if err2 != nil {
			return fmt.Errorf("insert closure: %w", err2)
		}

		// Suspend the account itself (accounts has no RLS — scope by id).
		if _, err := tx.ExecContext(ctx,
			`UPDATE accounts SET status = 'suspended', updated_at = now() WHERE id = $1::uuid`, accountID); err != nil {
			return fmt.Errorf("suspend account: %w", err)
		}
		return nil
	})
	if err != nil {
		return accountlifecycle.ClosureRequest{}, err
	}
	return out, nil
}

// CancelClosure reverses an open grace closure.
func (s *Store) CancelClosure(ctx context.Context, accountID string) (accountlifecycle.ClosureRequest, error) {
	var out accountlifecycle.ClosureRequest
	err := s.withTenant(ctx, accountID, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx,
			`SELECT `+closureCols+` FROM account_closure_requests
			 WHERE account_id = $1::uuid AND status = 'grace'`, accountID)
		req, err := scanClosure(row.Scan)
		if err == sql.ErrNoRows {
			return accountlifecycle.ErrNotClosing
		}
		if err != nil {
			return fmt.Errorf("load closure: %w", err)
		}

		// Restore exactly the rows this closure touched (and only if still in the
		// state we left them — a row the owner re-paused manually stays paused).
		if len(req.PausedLineItems) > 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE line_items SET status = 'live', updated_at = now()
				 WHERE account_id = $1::uuid AND status = 'paused' AND id = ANY($2::uuid[])`,
				accountID, pq.Array(req.PausedLineItems)); err != nil {
				return fmt.Errorf("restore line items: %w", err)
			}
		}
		if len(req.DeactivatedPlacements) > 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE placements SET status = 'active', updated_at = now()
				 WHERE account_id = $1::uuid AND status = 'inactive' AND id = ANY($2::uuid[])`,
				accountID, pq.Array(req.DeactivatedPlacements)); err != nil {
				return fmt.Errorf("restore placements: %w", err)
			}
		}

		row = tx.QueryRowContext(ctx, `
UPDATE account_closure_requests SET status = 'cancelled', cancelled_at = now(), updated_at = now()
WHERE id = $1::uuid RETURNING `+closureCols, req.ID)
		out, err = scanClosure(row.Scan)
		if err != nil {
			return fmt.Errorf("mark cancelled: %w", err)
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE accounts SET status = 'active', updated_at = now() WHERE id = $1::uuid`, accountID); err != nil {
			return fmt.Errorf("reactivate account: %w", err)
		}
		return nil
	})
	if err != nil {
		return accountlifecycle.ClosureRequest{}, err
	}
	return out, nil
}

// ActiveClosure returns the account's open grace closure, or nil.
func (s *Store) ActiveClosure(ctx context.Context, accountID string) (*accountlifecycle.ClosureRequest, error) {
	var out *accountlifecycle.ClosureRequest
	err := s.withTenant(ctx, accountID, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx,
			`SELECT `+closureCols+` FROM account_closure_requests
			 WHERE account_id = $1::uuid AND status = 'grace'`, accountID)
		req, err := scanClosure(row.Scan)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		out = &req
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("active closure: %w", err)
	}
	return out, nil
}

const closureCols = `id::text, account_id::text, status, COALESCE(reason,''),
       COALESCE(requested_by::text,''), requested_at, grace_ends_at,
       paused_line_items, deactivated_placements, cancelled_at, closed_at, updated_at`

func scanClosure(scan func(dest ...any) error) (accountlifecycle.ClosureRequest, error) {
	var c accountlifecycle.ClosureRequest
	var paused, deact pq.StringArray
	var cancelled, closed sql.NullTime
	if err := scan(&c.ID, &c.AccountID, &c.Status, &c.Reason, &c.RequestedBy,
		&c.RequestedAt, &c.GraceEndsAt, &paused, &deact, &cancelled, &closed, &c.UpdatedAt); err != nil {
		return accountlifecycle.ClosureRequest{}, err
	}
	c.PausedLineItems = paused
	c.DeactivatedPlacements = deact
	if cancelled.Valid {
		c.CancelledAt = &cancelled.Time
	}
	if closed.Valid {
		c.ClosedAt = &closed.Time
	}
	return c, nil
}

// collectIDs runs an UPDATE ... RETURNING id and gathers the ids.
func collectIDs(ctx context.Context, tx *sql.Tx, query, accountID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Non-nil so pq.Array yields '{}' (an empty array), not SQL NULL — the
	// columns are NOT NULL DEFAULT '{}'.
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
