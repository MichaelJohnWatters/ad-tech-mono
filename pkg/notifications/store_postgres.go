package notifications

import (
	"context"
	"database/sql"
)

// PostgresStore reads/writes the `notifications` table (migration 049). Every
// method sets the RLS tenant GUC (app.current_account_id) AND filters by
// account_id explicitly, so the row-level policy is the safety net behind an
// explicit filter — same pattern as the webhooks/conversion stores.
type PostgresStore struct{ db *sql.DB }

// NewPostgresStore wraps an open *sql.DB.
func NewPostgresStore(db *sql.DB) *PostgresStore { return &PostgresStore{db: db} }

// defaultListLimit bounds a list read when the caller passes a non-positive
// limit.
const defaultListLimit = 50

func (s *PostgresStore) Insert(ctx context.Context, n Notification) error {
	if s.db == nil {
		return sql.ErrConnDone
	}
	return s.withTenant(ctx, n.AccountID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO notifications (account_id, kind, title, body, ref_id)
			 VALUES ($1::uuid, $2, $3, NULLIF($4, ''), NULLIF($5, ''))`,
			n.AccountID, n.Kind, n.Title, n.Body, n.RefID)
		return err
	})
}

func (s *PostgresStore) ListForAccount(ctx context.Context, accountID string, limit int) ([]Notification, error) {
	out := []Notification{}
	if s.db == nil {
		return out, sql.ErrConnDone
	}
	if limit <= 0 {
		limit = defaultListLimit
	}
	// Tenant-scoped: set the caller's account GUC so RLS admits their rows under
	// the NOBYPASSRLS app role (security #77). Read-only tx held open for scan.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, accountID); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT id::text, account_id::text, kind, title, COALESCE(body, ''),
		        COALESCE(ref_id, ''), read, created_at
		   FROM notifications
		  WHERE account_id = $1::uuid
		  ORDER BY created_at DESC
		  LIMIT $2`,
		accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var n Notification
		if err := rows.Scan(&n.ID, &n.AccountID, &n.Kind, &n.Title, &n.Body,
			&n.RefID, &n.Read, &n.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *PostgresStore) UnreadCount(ctx context.Context, accountID string) (int, error) {
	if s.db == nil {
		return 0, sql.ErrConnDone
	}
	var count int
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, accountID); err != nil {
		return 0, err
	}
	err = tx.QueryRowContext(ctx,
		`SELECT count(*) FROM notifications WHERE account_id = $1::uuid AND read = false`,
		accountID).Scan(&count)
	return count, err
}

func (s *PostgresStore) MarkRead(ctx context.Context, accountID, id string) error {
	if s.db == nil {
		return sql.ErrConnDone
	}
	return s.withTenant(ctx, accountID, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE notifications SET read = true WHERE id = $1::uuid AND account_id = $2::uuid`,
			id, accountID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return sql.ErrNoRows
		}
		return nil
	})
}

func (s *PostgresStore) MarkAllRead(ctx context.Context, accountID string) error {
	if s.db == nil {
		return sql.ErrConnDone
	}
	return s.withTenant(ctx, accountID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE notifications SET read = true WHERE account_id = $1::uuid AND read = false`,
			accountID)
		return err
	})
}

// withTenant runs fn in a transaction with the RLS tenant GUC set so writes are
// admitted (and scoped to accountID).
func (s *PostgresStore) withTenant(ctx context.Context, accountID string, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, accountID); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
