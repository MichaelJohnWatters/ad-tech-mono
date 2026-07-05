package webhooks

import (
	"context"
	"database/sql"
)

// PostgresStore reads webhook subscriptions and writes delivery records against
// the `webhooks` and `webhook_deliveries` tables. Subscription reads are
// tenant-scoped: the query filters by account_id explicitly AND sets the RLS
// context (app.current_account_id) so the row-level policy is the safety net.
type PostgresStore struct{ db *sql.DB }

// NewPostgresStore wraps an open *sql.DB.
func NewPostgresStore(db *sql.DB) *PostgresStore { return &PostgresStore{db: db} }

func (s *PostgresStore) ActiveForEvent(ctx context.Context, accountID, eventType string) ([]Subscription, error) {
	if s.db == nil {
		return nil, sql.ErrConnDone
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, accountID); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT id::text, url, secret
		   FROM webhooks
		  WHERE account_id = $1::uuid
		    AND status = 'active'
		    AND $2 = ANY(events)`,
		accountID, eventType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Subscription
	for rows.Next() {
		var sub Subscription
		if err := rows.Scan(&sub.ID, &sub.URL, &sub.Secret); err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

func (s *PostgresStore) RecordDelivery(ctx context.Context, d Delivery) error {
	if s.db == nil {
		return sql.ErrConnDone
	}
	// response_status is nullable (0 = transport error, no HTTP response).
	var status sql.NullInt32
	if d.ResponseStatus > 0 {
		status = sql.NullInt32{Int32: int32(d.ResponseStatus), Valid: true}
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO webhook_deliveries
		   (webhook_id, event_type, payload, response_status, response_body, attempt, success)
		 VALUES ($1::uuid, $2, $3::jsonb, $4, $5, $6, $7)`,
		d.WebhookID, d.EventType, string(d.Payload), status, d.ResponseBody, d.Attempt, d.Success)
	return err
}
