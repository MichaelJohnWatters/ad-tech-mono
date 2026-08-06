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

// FetchAll returns every config row in the table — used by admin tooling
// and the config manager UI. Each pod uses FetchAllForPod for its own
// scoped read on poll.
func (s *PostgresSource) FetchAll(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT key, value FROM config")
	if err != nil {
		return nil, fmt.Errorf("query config: %w", err)
	}
	defer rows.Close()
	return scanKV(rows)
}

// FetchAllForPod returns the rows visible to a single pod: its own pod_id
// rows + legacy global rows (pod_id = ”). When the same key has both,
// pod-specific wins. The manager polls this so each pod's in-memory map
// only contains values that apply to *it*.
func (s *PostgresSource) FetchAllForPod(ctx context.Context, podID string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT key, value, pod_id FROM config
		WHERE pod_id = $1 OR pod_id = ''
	`, podID)
	if err != nil {
		return nil, fmt.Errorf("query config for pod %s: %w", podID, err)
	}
	defer rows.Close()

	// Two-pass merge: collect globals first, then overlay pod-specific.
	// One pass with "pod-specific overrides" would also work but is harder
	// to read.
	globals := map[string]string{}
	specifics := map[string]string{}
	for rows.Next() {
		var key, scope string
		var valueJSON []byte
		if err := rows.Scan(&key, &valueJSON, &scope); err != nil {
			return nil, fmt.Errorf("scan config row: %w", err)
		}
		val := decodeJSONStringOrRaw(valueJSON)
		if scope == "" {
			globals[key] = val
		} else {
			specifics[key] = val
		}
	}
	for k, v := range specifics {
		globals[k] = v
	}
	return globals, rows.Err()
}

func (s *PostgresSource) Update(ctx context.Context, key, value string) error {
	return s.UpdateForPod(ctx, "", key, value, "config_update")
}

// UpdateForPod writes a config row for a specific pod (use podID="" for the
// legacy global row). Action labels are: config_update, config_rollback,
// config_reset, config_seed.
func (s *PostgresSource) UpdateForPod(ctx context.Context, podID, key, value, action string) error {
	valueJSON, _ := json.Marshal(value)

	// Old value for the audit trail (look up *this pod's* row).
	var oldValueJSON []byte
	s.db.QueryRowContext(ctx,
		"SELECT value FROM config WHERE pod_id = $1 AND key = $2",
		podID, key,
	).Scan(&oldValueJSON)
	var oldVal string
	if len(oldValueJSON) > 0 {
		if err := json.Unmarshal(oldValueJSON, &oldVal); err != nil {
			oldVal = string(oldValueJSON)
		}
	}
	if oldVal == "" {
		for _, entry := range Schema() {
			if entry.Key == key {
				oldVal = entry.Default
				break
			}
		}
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO config (pod_id, key, value, service, updated_at)
		VALUES ($1, $2, $3, 'platform', now())
		ON CONFLICT (pod_id, key) DO UPDATE SET value = $3, updated_at = now()
	`, podID, key, valueJSON)
	if err != nil {
		return err
	}

	// Audit row records the pod scope so history shows "applied to pod X".
	changesJSON, _ := json.Marshal(map[string]string{
		"old_value": oldVal,
		"new_value": value,
		"pod_id":    podID,
	})
	s.db.ExecContext(ctx, `
		INSERT INTO audit_log (account_id, actor_id, action, resource_type, resource_id, changes, timestamp)
		VALUES (NULL, 'config_manager', $1, 'config', $2, $3, now())
	`, action, key, changesJSON)

	return nil
}

// UpdateWithAction is the legacy signature kept for compatibility with the
// UI's bulk endpoints. Writes to the global scope (pod_id = ”).
func (s *PostgresSource) UpdateWithAction(ctx context.Context, key, value, action string) error {
	return s.UpdateForPod(ctx, "", key, value, action)
}

func (s *PostgresSource) Delete(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM config WHERE key = $1", key)
	return err
}

// DeleteForPod removes a specific pod's row for key (or all rows for key
// when podID is empty).
func (s *PostgresSource) DeleteForPod(ctx context.Context, podID, key string) error {
	_, err := s.db.ExecContext(ctx,
		"DELETE FROM config WHERE pod_id = $1 AND key = $2",
		podID, key,
	)
	return err
}

// scanKV materialises rows produced by `SELECT key, value FROM config`
// without a pod_id column. Used by FetchAll (admin tooling).
func scanKV(rows *sql.Rows) (map[string]string, error) {
	values := make(map[string]string)
	for rows.Next() {
		var key string
		var valueJSON []byte
		if err := rows.Scan(&key, &valueJSON); err != nil {
			return nil, fmt.Errorf("scan config row: %w", err)
		}
		values[key] = decodeJSONStringOrRaw(valueJSON)
	}
	return values, rows.Err()
}

func decodeJSONStringOrRaw(b []byte) string {
	var val string
	if err := json.Unmarshal(b, &val); err != nil {
		return string(b)
	}
	return val
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
