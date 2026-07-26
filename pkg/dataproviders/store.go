package dataproviders

import (
	"context"
	"database/sql"
	"errors"

	"github.com/lib/pq"
)

// PostgresStore is the data_providers store. Writes set the RLS context
// (app.current_account_id) in a transaction — mirroring pkg/audiencemappings —
// so inserts/updates pass the tenant_isolation policy under a non-bypass role.
// Reads carry an explicit account_id filter (RLS is the safety net).
type PostgresStore struct {
	DB *sql.DB
}

// NewPostgresStore returns a Store backed by db.
func NewPostgresStore(db *sql.DB) *PostgresStore { return &PostgresStore{DB: db} }

const providerColumns = `id::text, account_id::text, name, kind, default_party,
	default_licence, default_id_type, encryption_expected, notify_emails, status,
	created_at, updated_at`

func scanProvider(scan func(dest ...any) error) (*Provider, error) {
	var p Provider
	if err := scan(&p.ID, &p.AccountID, &p.Name, &p.Kind, &p.DefaultParty,
		&p.DefaultLicence, &p.DefaultIDType, &p.EncryptionExpected,
		pq.Array(&p.NotifyEmails), &p.Status, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	if p.NotifyEmails == nil {
		p.NotifyEmails = []string{}
	}
	return &p, nil
}

// Create inserts the account's provider (or updates it on a name conflict) and
// returns its id. The caller should Normalize + Validate before calling.
func (s *PostgresStore) Create(ctx context.Context, p Provider) (string, error) {
	if s.DB == nil {
		return "", sql.ErrConnDone
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`SELECT set_config('app.current_account_id', $1, true)`, p.AccountID); err != nil {
		return "", err
	}
	var id string
	err = tx.QueryRowContext(ctx, `
INSERT INTO data_providers
	(account_id, name, kind, default_party, default_licence, default_id_type,
	 encryption_expected, notify_emails, status)
VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (account_id, name) DO UPDATE SET
	kind = EXCLUDED.kind,
	default_party = EXCLUDED.default_party,
	default_licence = EXCLUDED.default_licence,
	default_id_type = EXCLUDED.default_id_type,
	encryption_expected = EXCLUDED.encryption_expected,
	notify_emails = EXCLUDED.notify_emails,
	status = EXCLUDED.status,
	updated_at = now()
RETURNING id::text`,
		p.AccountID, p.Name, p.Kind, p.DefaultParty, p.DefaultLicence,
		p.DefaultIDType, p.EncryptionExpected, pq.Array(p.NotifyEmails), p.Status).Scan(&id)
	if err != nil {
		return "", err
	}
	return id, tx.Commit()
}

// ListByAccount returns the account's providers, newest first.
func (s *PostgresStore) ListByAccount(ctx context.Context, accountID string) ([]Provider, error) {
	if s.DB == nil {
		return nil, sql.ErrConnDone
	}
	// Tenant-scoped: set the caller's account GUC so RLS admits their rows under
	// the NOBYPASSRLS app role (security #77). Read-only tx keeps the *sql.Rows
	// valid while scanning and auto-resets the GUC.
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, accountID); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT `+providerColumns+` FROM data_providers
		 WHERE account_id = $1::uuid ORDER BY created_at DESC`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Provider{}
	for rows.Next() {
		p, err := scanProvider(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// GetByAccount returns one provider only if it belongs to the account.
func (s *PostgresStore) GetByAccount(ctx context.Context, accountID, id string) (*Provider, error) {
	if s.DB == nil {
		return nil, sql.ErrConnDone
	}
	// Tenant-scoped: set the caller's account GUC so RLS admits the row under the
	// NOBYPASSRLS app role (security #77). scan runs inside the read-only tx.
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, accountID); err != nil {
		return nil, err
	}
	row := tx.QueryRowContext(ctx,
		`SELECT `+providerColumns+` FROM data_providers
		 WHERE id = $1::uuid AND account_id = $2::uuid`, id, accountID)
	p, err := scanProvider(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

// DeleteByAccount removes one provider scoped to the account. The provider_id FKs
// on audience_mappings/audience_ingest_jobs/audience_segments are ON DELETE SET
// NULL, so deleting a provider detaches (never cascades-away) its history.
func (s *PostgresStore) DeleteByAccount(ctx context.Context, accountID, id string) error {
	if s.DB == nil {
		return sql.ErrConnDone
	}
	_, err := s.DB.ExecContext(ctx,
		`DELETE FROM data_providers WHERE id = $1::uuid AND account_id = $2::uuid`, id, accountID)
	return err
}
