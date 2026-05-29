package config

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"time"
)

// ServiceRegistration is what each service writes to Postgres on startup.
type ServiceRegistration struct {
	Service    string    `json:"service"`
	Version    string    `json:"version"`
	Host       string    `json:"host"`
	Port       string    `json:"port"`
	ConfigKeys []string  `json:"config_keys"` // which keys this service uses
	Status     string    `json:"status"`       // running, stopped
	StartedAt  time.Time `json:"started_at"`
	LastPingAt time.Time `json:"last_ping_at"`
}

// Registry tracks which services are running and their config keys.
type Registry struct {
	db  *sql.DB
	log *slog.Logger
}

// NewRegistry creates a service registry backed by Postgres.
func NewRegistry(db *sql.DB, log *slog.Logger) *Registry {
	return &Registry{db: db, log: log}
}

// Register announces this service to the registry.
// Creates its config keys if they don't exist.
func (r *Registry) Register(ctx context.Context, serviceName, version, port string) error {
	if r.db == nil {
		return nil
	}

	host, _ := os.Hostname()

	// Upsert into service_registry (create table if not exists)
	_, err := r.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS service_registry (
			service TEXT PRIMARY KEY,
			version TEXT,
			host TEXT,
			port TEXT,
			config_keys JSONB DEFAULT '[]',
			status TEXT DEFAULT 'running',
			started_at TIMESTAMPTZ DEFAULT now(),
			last_ping_at TIMESTAMPTZ DEFAULT now()
		)
	`)
	if err != nil {
		return err
	}

	// Get config keys for this service from schema
	var keys []string
	for _, entry := range Schema() {
		if entry.Service == serviceName {
			keys = append(keys, entry.Key)
		}
	}
	keysJSON, _ := json.Marshal(keys)

	_, err = r.db.ExecContext(ctx, `
		INSERT INTO service_registry (service, version, host, port, config_keys, status, started_at, last_ping_at)
		VALUES ($1, $2, $3, $4, $5, 'running', now(), now())
		ON CONFLICT (service) DO UPDATE SET
			version = $2, host = $3, port = $4, config_keys = $5,
			status = 'running', last_ping_at = now()
	`, serviceName, version, host, port, keysJSON)
	if err != nil {
		return err
	}

	// Ensure all config keys exist in the config table
	source := NewPostgresSource(r.db)
	for _, entry := range Schema() {
		if entry.Service != serviceName {
			continue
		}
		// Only insert if not exists (don't overwrite existing values)
		existing, _ := r.db.QueryContext(ctx, "SELECT 1 FROM config WHERE key = $1", entry.Key)
		if existing != nil {
			hasRow := existing.Next()
			existing.Close()
			if hasRow {
				continue // already exists, don't overwrite
			}
		}
		source.Update(ctx, entry.Key, entry.Default)
		r.log.Debug("seeded config key", "key", entry.Key, "default", entry.Default)
	}

	r.log.Info("service registered",
		"service", serviceName,
		"version", version,
		"port", port,
		"config_keys", len(keys),
	)
	return nil
}

// Deregister marks a service as stopped.
func (r *Registry) Deregister(ctx context.Context, serviceName string) error {
	if r.db == nil {
		return nil
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE service_registry SET status = 'stopped', last_ping_at = now()
		WHERE service = $1
	`, serviceName)
	return err
}

// Ping updates the last_ping_at timestamp (call periodically to show service is alive).
func (r *Registry) Ping(ctx context.Context, serviceName string) error {
	if r.db == nil {
		return nil
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE service_registry SET last_ping_at = now()
		WHERE service = $1
	`, serviceName)
	return err
}

// ListServices returns all registered services.
func (r *Registry) ListServices(ctx context.Context) ([]ServiceRegistration, error) {
	if r.db == nil {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT service, version, host, port, config_keys, status, started_at, last_ping_at
		FROM service_registry ORDER BY service
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []ServiceRegistration
	for rows.Next() {
		var s ServiceRegistration
		var keysJSON []byte
		if err := rows.Scan(&s.Service, &s.Version, &s.Host, &s.Port, &keysJSON, &s.Status, &s.StartedAt, &s.LastPingAt); err != nil {
			continue
		}
		json.Unmarshal(keysJSON, &s.ConfigKeys)
		result = append(result, s)
	}
	return result, nil
}

// HTTPHandler returns an endpoint that shows all registered services and their config.
func (r *Registry) HTTPHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r2 *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		services, err := r.ListServices(r2.Context())
		if err != nil {
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(services)
	}
}
