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
	return s.UpdateWithAction(ctx, key, value, "config_update")
}

// UpdateWithAction writes a config value and records the action type in the audit log.
// Actions: config_update, config_rollback, config_reset
func (s *PostgresSource) UpdateWithAction(ctx context.Context, key, value, action string) error {
	valueJSON, _ := json.Marshal(value)

	// Get old value for audit trail
	var oldValueJSON []byte
	s.db.QueryRowContext(ctx, "SELECT value FROM config WHERE key = $1", key).Scan(&oldValueJSON)
	var oldVal string
	if len(oldValueJSON) > 0 {
		if err := json.Unmarshal(oldValueJSON, &oldVal); err != nil {
			oldVal = string(oldValueJSON)
		}
	}
	// If no existing value, use the schema default so audit log shows what it was
	if oldVal == "" {
		for _, entry := range Schema() {
			if entry.Key == key {
				oldVal = entry.Default
				break
			}
		}
	}

	// Upsert
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO config (key, value, service, updated_at)
		VALUES ($1, $2, 'platform', now())
		ON CONFLICT (key) DO UPDATE SET value = $2, updated_at = now()
	`, key, valueJSON)
	if err != nil {
		return err
	}

	// Write to audit log with action type
	changesJSON, _ := json.Marshal(map[string]string{
		"old_value": oldVal,
		"new_value": value,
	})
	s.db.ExecContext(ctx, `
		INSERT INTO audit_log (account_id, actor_id, action, resource_type, resource_id, changes, timestamp)
		VALUES (NULL, 'config_manager', $1, 'config', $2, $3, now())
	`, action, key, changesJSON)

	return nil
}

func (s *PostgresSource) Delete(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM config WHERE key = $1", key)
	return err
}

// ConfigChangeLog is a persistent config change record from the audit_log table.
type ConfigChangeLog struct {
	ID        string    `json:"id"`
	Key       string    `json:"key"`
	Action    string    `json:"action"` // config_update, config_rollback, config_reset
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
			SELECT id, resource_id, action, changes, actor_id, timestamp
			FROM audit_log
			WHERE resource_type = 'config' AND resource_id = $1
			ORDER BY timestamp DESC LIMIT $2
		`, key, limit)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, resource_id, action, changes, actor_id, timestamp
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
		if err := rows.Scan(&cl.ID, &cl.Key, &cl.Action, &changesJSON, &cl.Actor, &cl.Timestamp); err != nil {
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
