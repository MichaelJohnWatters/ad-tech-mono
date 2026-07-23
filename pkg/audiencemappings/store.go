package audiencemappings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// PostgresStore is the audience_mappings store. Every write sets the RLS context
// (app.current_account_id) in a transaction — mirroring pkg/reportjobs.Enqueue —
// so inserts/updates pass the tenant_isolation policy under a non-bypass role.
// Reads carry an explicit account_id filter (RLS is the safety net, not the
// mechanism).
type PostgresStore struct {
	DB *sql.DB
}

// NewPostgresStore returns a Store backed by db.
func NewPostgresStore(db *sql.DB) *PostgresStore { return &PostgresStore{DB: db} }

const mappingColumns = `id::text, account_id::text, name, mappings::text, id_type, created_at, updated_at`

func scanMapping(scan func(dest ...any) error) (*Mapping, error) {
	var m Mapping
	var mappingsJSON string
	if err := scan(&m.ID, &m.AccountID, &m.Name, &mappingsJSON, &m.IDType, &m.CreatedAt, &m.UpdatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(mappingsJSON), &m.Mappings); err != nil {
		return nil, fmt.Errorf("decode mappings for %s: %w", m.ID, err)
	}
	return &m, nil
}

// Create inserts the account's mapping (or updates it on a name conflict) and
// returns its id. The RLS context is set to the mapping's account so the write
// passes tenant policy under a non-bypass role.
func (s *PostgresStore) Create(ctx context.Context, m Mapping) (string, error) {
	if s.DB == nil {
		return "", sql.ErrConnDone
	}
	mappingsJSON, err := json.Marshal(m.Mappings)
	if err != nil {
		return "", fmt.Errorf("encode mappings: %w", err)
	}
	idType := m.IDType
	if idType == "" {
		idType = "user_id"
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`SELECT set_config('app.current_account_id', $1, true)`, m.AccountID); err != nil {
		return "", err
	}
	var id string
	err = tx.QueryRowContext(ctx, `
INSERT INTO audience_mappings (account_id, name, mappings, id_type)
VALUES ($1::uuid, $2, $3::jsonb, $4)
ON CONFLICT (account_id, name) DO UPDATE
    SET mappings = EXCLUDED.mappings, id_type = EXCLUDED.id_type, updated_at = now()
RETURNING id::text`, m.AccountID, m.Name, string(mappingsJSON), idType).Scan(&id)
	if err != nil {
		return "", err
	}
	return id, tx.Commit()
}

// ListByAccount returns the account's mappings, newest first.
func (s *PostgresStore) ListByAccount(ctx context.Context, accountID string) ([]Mapping, error) {
	if s.DB == nil {
		return nil, sql.ErrConnDone
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT `+mappingColumns+` FROM audience_mappings
		 WHERE account_id = $1::uuid ORDER BY created_at DESC`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Mapping{}
	for rows.Next() {
		m, err := scanMapping(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// GetByAccount returns one mapping only if it belongs to the account.
func (s *PostgresStore) GetByAccount(ctx context.Context, accountID, id string) (*Mapping, error) {
	if s.DB == nil {
		return nil, sql.ErrConnDone
	}
	row := s.DB.QueryRowContext(ctx,
		`SELECT `+mappingColumns+` FROM audience_mappings
		 WHERE id = $1::uuid AND account_id = $2::uuid`, id, accountID)
	m, err := scanMapping(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return m, err
}

// DeleteByAccount removes one mapping scoped to the account.
func (s *PostgresStore) DeleteByAccount(ctx context.Context, accountID, id string) error {
	if s.DB == nil {
		return sql.ErrConnDone
	}
	_, err := s.DB.ExecContext(ctx,
		`DELETE FROM audience_mappings WHERE id = $1::uuid AND account_id = $2::uuid`, id, accountID)
	return err
}
