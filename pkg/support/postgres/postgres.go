// Package postgres implements support.Store. Customer methods run in a tenant
// transaction (RLS GUC = the account); the staff queue + resolve run under the
// platform hatch (cross-tenant).
package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/support"
)

// Store is the Postgres-backed support store.
type Store struct{ db *sql.DB }

// New returns a Store backed by db.
func New(db *sql.DB) *Store { return &Store{db: db} }

func (s *Store) tenantTx(ctx context.Context, accountID string, fn func(tx *sql.Tx) error) error {
	return s.txWith(ctx, `SELECT set_config('app.current_account_id', $1, true)`, []any{accountID}, fn)
}

func (s *Store) platformTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	return s.txWith(ctx, `SELECT set_config('app.platform_read', 'on', true)`, nil, fn)
}

func (s *Store) txWith(ctx context.Context, setSQL string, args []any, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	if _, err := tx.ExecContext(ctx, setSQL, args...); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("set guc: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

const ticketCols = `id::text, account_id::text, kind, subject, status,
       amount_disputed_micros, currency, COALESCE(resolution,''), COALESCE(assigned_to,''),
       COALESCE(created_by,''), created_at, updated_at, resolved_at`

func scanTicket(scan func(dest ...any) error) (support.Ticket, error) {
	var t support.Ticket
	var amount sql.NullInt64
	var resolved sql.NullTime
	if err := scan(&t.ID, &t.AccountID, &t.Kind, &t.Subject, &t.Status,
		&amount, &t.Currency, &t.Resolution, &t.AssignedTo, &t.CreatedBy,
		&t.CreatedAt, &t.UpdatedAt, &resolved); err != nil {
		return support.Ticket{}, err
	}
	if amount.Valid {
		t.AmountDisputedMicros = &amount.Int64
	}
	if resolved.Valid {
		t.ResolvedAt = &resolved.Time
	}
	return t, nil
}

// Create opens a ticket + optional opening message in one tenant tx.
func (s *Store) Create(ctx context.Context, t support.Ticket, openingMessage string) (string, error) {
	if t.Currency == "" {
		t.Currency = "USD"
	}
	var id string
	err := s.tenantTx(ctx, t.AccountID, func(tx *sql.Tx) error {
		var amount any
		if t.AmountDisputedMicros != nil {
			amount = *t.AmountDisputedMicros
		}
		var createdBy any
		if t.CreatedBy != "" {
			createdBy = t.CreatedBy
		}
		if err := tx.QueryRowContext(ctx, `
INSERT INTO support_tickets (account_id, kind, subject, status, amount_disputed_micros, currency, created_by)
VALUES ($1::uuid, $2, $3, 'open', $4, $5, $6)
RETURNING id::text`,
			t.AccountID, t.Kind, t.Subject, amount, t.Currency, createdBy).Scan(&id); err != nil {
			return fmt.Errorf("insert ticket: %w", err)
		}
		if openingMessage != "" {
			if _, err := tx.ExecContext(ctx, `
INSERT INTO support_messages (ticket_id, author_type, author_id, body)
VALUES ($1::uuid, 'customer', $2, $3)`, id, createdBy, openingMessage); err != nil {
				return fmt.Errorf("insert opening message: %w", err)
			}
		}
		return nil
	})
	return id, err
}

func (s *Store) listTickets(ctx context.Context, tenant bool, accountID, where string, args ...any) ([]support.Ticket, error) {
	out := []support.Ticket{}
	scan := func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+ticketCols+` FROM support_tickets `+where, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			t, err := scanTicket(rows.Scan)
			if err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	}
	var err error
	if tenant {
		err = s.tenantTx(ctx, accountID, scan)
	} else {
		err = s.platformTx(ctx, scan)
	}
	if err != nil {
		return nil, fmt.Errorf("list tickets: %w", err)
	}
	return out, nil
}

// ListByAccount returns the account's own tickets (tenant).
func (s *Store) ListByAccount(ctx context.Context, accountID string, limit int) ([]support.Ticket, error) {
	if limit <= 0 {
		limit = 100
	}
	return s.listTickets(ctx, true, accountID,
		`WHERE account_id = $1::uuid ORDER BY created_at DESC LIMIT $2`, accountID, limit)
}

// ListAll returns every ticket for the staff queue with the account name (hatch).
func (s *Store) ListAll(ctx context.Context, limit int) ([]support.Ticket, error) {
	if limit <= 0 {
		limit = 200
	}
	out := []support.Ticket{}
	err := s.platformTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
SELECT `+ticketColsPrefixed("t")+`, COALESCE(a.name,'')
FROM support_tickets t JOIN accounts a ON a.id = t.account_id
ORDER BY (t.status IN ('open','pending')) DESC, t.created_at DESC LIMIT $1`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t support.Ticket
			var amount sql.NullInt64
			var resolved sql.NullTime
			if err := rows.Scan(&t.ID, &t.AccountID, &t.Kind, &t.Subject, &t.Status,
				&amount, &t.Currency, &t.Resolution, &t.AssignedTo, &t.CreatedBy,
				&t.CreatedAt, &t.UpdatedAt, &resolved, &t.AccountName); err != nil {
				return err
			}
			if amount.Valid {
				t.AmountDisputedMicros = &amount.Int64
			}
			if resolved.Valid {
				t.ResolvedAt = &resolved.Time
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list all tickets: %w", err)
	}
	return out, nil
}

// GetForAccount returns one of the account's tickets + thread (tenant).
func (s *Store) GetForAccount(ctx context.Context, accountID, id string) (*support.Ticket, error) {
	var out *support.Ticket
	err := s.tenantTx(ctx, accountID, func(tx *sql.Tx) error {
		t, err := getTicketTx(ctx, tx, `WHERE id = $1::uuid AND account_id = $2::uuid`, id, accountID)
		out = t
		return err
	})
	return out, err
}

// GetAny returns any ticket + thread for staff (platform hatch).
func (s *Store) GetAny(ctx context.Context, id string) (*support.Ticket, error) {
	var out *support.Ticket
	err := s.platformTx(ctx, func(tx *sql.Tx) error {
		t, err := getTicketTx(ctx, tx, `WHERE id = $1::uuid`, id)
		out = t
		return err
	})
	return out, err
}

func getTicketTx(ctx context.Context, tx *sql.Tx, where string, args ...any) (*support.Ticket, error) {
	t, err := scanTicket(tx.QueryRowContext(ctx, `SELECT `+ticketCols+` FROM support_tickets `+where, args...).Scan)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get ticket: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `
SELECT id::text, ticket_id::text, author_type, COALESCE(author_id,''), body, created_at
FROM support_messages WHERE ticket_id = $1::uuid ORDER BY created_at`, t.ID)
	if err != nil {
		return nil, fmt.Errorf("get messages: %w", err)
	}
	defer rows.Close()
	t.Messages = []support.Message{}
	for rows.Next() {
		var m support.Message
		if err := rows.Scan(&m.ID, &m.TicketID, &m.AuthorType, &m.AuthorID, &m.Body, &m.CreatedAt); err != nil {
			return nil, err
		}
		t.Messages = append(t.Messages, m)
	}
	return &t, rows.Err()
}

// AddCustomerMessage appends a customer reply and reopens the ticket (tenant).
func (s *Store) AddCustomerMessage(ctx context.Context, accountID, ticketID, authorID, body string) error {
	return s.tenantTx(ctx, accountID, func(tx *sql.Tx) error {
		return addMessageTx(ctx, tx, ticketID, support.AuthorCustomer, authorID, body, support.StatusOpen)
	})
}

// AddStaffMessage appends a staff reply and marks the ticket pending (hatch).
func (s *Store) AddStaffMessage(ctx context.Context, ticketID, authorID, body string) error {
	return s.platformTx(ctx, func(tx *sql.Tx) error {
		return addMessageTx(ctx, tx, ticketID, support.AuthorStaff, authorID, body, support.StatusPending)
	})
}

// addMessageTx inserts a message and moves the ticket to newStatus unless it is
// already resolved/closed by the OTHER party in a terminal way — a resolved
// ticket reopens on a customer reply (newStatus=open) but a staff note keeps it
// pending; simplest rule: always set newStatus (a customer reopening a resolved
// ticket is desirable, and staff replying to an open one setting pending is fine).
func addMessageTx(ctx context.Context, tx *sql.Tx, ticketID, authorType, authorID, body, newStatus string) error {
	res, err := tx.ExecContext(ctx, `
INSERT INTO support_messages (ticket_id, author_type, author_id, body)
SELECT $1::uuid, $2, $3, $4 WHERE EXISTS (SELECT 1 FROM support_tickets WHERE id = $1::uuid)`,
		ticketID, authorType, authorID, body)
	if err != nil {
		return fmt.Errorf("insert message: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("ticket not found or not visible")
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE support_tickets SET status = $2, updated_at = now(),
    resolved_at = CASE WHEN $2 = 'resolved' THEN resolved_at ELSE NULL END
WHERE id = $1::uuid`, ticketID, newStatus); err != nil {
		return fmt.Errorf("touch ticket: %w", err)
	}
	return nil
}

// Resolve sets a ticket's status/resolution/assignee (platform hatch, staff).
func (s *Store) Resolve(ctx context.Context, ticketID, status, resolution, assignedTo string) (*support.Ticket, error) {
	var out *support.Ticket
	err := s.platformTx(ctx, func(tx *sql.Tx) error {
		var assigned any
		if assignedTo != "" {
			assigned = assignedTo
		}
		t, err := scanTicket(tx.QueryRowContext(ctx, `
UPDATE support_tickets SET
    status = $2, resolution = $3, assigned_to = COALESCE($4, assigned_to), updated_at = now(),
    resolved_at = CASE WHEN $2 IN ('resolved','closed') THEN COALESCE(resolved_at, now()) ELSE NULL END
WHERE id = $1::uuid
RETURNING `+ticketCols, ticketID, status, resolution, assigned).Scan)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return fmt.Errorf("resolve ticket: %w", err)
		}
		out = &t
		return nil
	})
	return out, err
}

// ticketColsPrefixed returns ticketCols with a table alias for JOIN selects.
func ticketColsPrefixed(alias string) string {
	return alias + `.id::text, ` + alias + `.account_id::text, ` + alias + `.kind, ` + alias + `.subject, ` + alias + `.status,
       ` + alias + `.amount_disputed_micros, ` + alias + `.currency, COALESCE(` + alias + `.resolution,''), COALESCE(` + alias + `.assigned_to,''),
       COALESCE(` + alias + `.created_by,''), ` + alias + `.created_at, ` + alias + `.updated_at, ` + alias + `.resolved_at`
}
