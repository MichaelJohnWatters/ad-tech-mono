package config

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// PostgresSource reads/writes config from the Postgres `config` table.
// Schema: key TEXT PRIMARY KEY, value JSONB, service TEXT, updated_at TIMESTAMPTZ
type PostgresSource struct {
	db *sql.DB
}

// NewPostgresSource creates a Postgres-backed config source.
func NewPostgresSource(db *sql.DB) *PostgresSource {
	return &PostgresSource{db: db}
}

func (s *PostgresSource) FetchAll(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT key, value FROM config")
	if err != nil {
		return nil, fmt.Errorf("query config: %w", err)
	}
	defer rows.Close()

	values := make(map[string]string)
	for rows.Next() {
		var key string
		var valueJSON []byte
		if err := rows.Scan(&key, &valueJSON); err != nil {
			return nil, fmt.Errorf("scan config row: %w", err)
		}
		// JSONB value - unwrap the JSON string
		var val string
		if err := json.Unmarshal(valueJSON, &val); err != nil {
			// Not a JSON string, use raw
			val = string(valueJSON)
		}
		values[key] = val
	}
	return values, rows.Err()
}

func (s *PostgresSource) Update(ctx context.Context, key, value string) error {
	valueJSON, _ := json.Marshal(value)

	// Get old value for audit trail
	var oldValueJSON []byte
	s.db.QueryRowContext(ctx, "SELECT value FROM config WHERE key = $1", key).Scan(&oldValueJSON)

	// Upsert
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO config (key, value, service, updated_at)
		VALUES ($1, $2, 'platform', now())
		ON CONFLICT (key) DO UPDATE SET value = $2, updated_at = now()
	`, key, valueJSON)
	if err != nil {
		return err
	}

	// Write to audit log for persistent history
	oldVal := string(oldValueJSON)
	changesJSON, _ := json.Marshal(map[string]string{
		"old_value": oldVal,
		"new_value": string(valueJSON),
	})
	s.db.ExecContext(ctx, `
		INSERT INTO audit_log (account_id, actor_id, action, resource_type, resource_id, changes, timestamp)
		VALUES (NULL, 'config_manager', 'config_update', 'config', $1, $2, now())
	`, key, changesJSON)

	return nil
}

func (s *PostgresSource) Delete(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM config WHERE key = $1", key)
	return err
}

// ConfigChangeLog is a persistent config change record from the audit_log table.
type ConfigChangeLog struct {
	Key       string    `json:"key"`
	OldValue  string    `json:"old_value"`
	NewValue  string    `json:"new_value"`
	Actor     string    `json:"actor"`
	Timestamp time.Time `json:"timestamp"`
}

// History returns the persistent change history for a config key from the audit log.
// If key is empty, returns all config changes.
func (s *PostgresSource) History(ctx context.Context, key string, limit int) ([]ConfigChangeLog, error) {
	var rows *sql.Rows
	var err error

	if key != "" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT resource_id, changes, actor_id, timestamp
			FROM audit_log
			WHERE resource_type = 'config' AND resource_id = $1
			ORDER BY timestamp DESC LIMIT $2
		`, key, limit)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT resource_id, changes, actor_id, timestamp
			FROM audit_log
			WHERE resource_type = 'config'
			ORDER BY timestamp DESC LIMIT $1
		`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []ConfigChangeLog
	for rows.Next() {
		var cl ConfigChangeLog
		var changesJSON []byte
		if err := rows.Scan(&cl.Key, &changesJSON, &cl.Actor, &cl.Timestamp); err != nil {
			continue
		}
		var changes map[string]string
		json.Unmarshal(changesJSON, &changes)
		cl.OldValue = changes["old_value"]
		cl.NewValue = changes["new_value"]
		result = append(result, cl)
	}
	return result, nil
}
