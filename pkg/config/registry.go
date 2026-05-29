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

// PodRegistration is what each pod writes to Postgres on startup.
type PodRegistration struct {
	PodID      string    `json:"pod_id"`
	Service    string    `json:"service"`
	Version    string    `json:"version"`
	Host       string    `json:"host"`
	Port       string    `json:"port"`
	ConfigKeys []string  `json:"config_keys"`
	Status     string    `json:"status"` // running, stopped
	StartedAt  time.Time `json:"started_at"`
	LastPingAt time.Time `json:"last_ping_at"`
}

// ServiceGroup is a service with all its running pods.
type ServiceGroup struct {
	Service    string            `json:"service"`
	Pods       []PodRegistration `json:"pods"`
	ConfigKeys []string          `json:"config_keys"`
}

// Registry tracks which pods are running and their config keys.
type Registry struct {
	db    *sql.DB
	log   *slog.Logger
	podID string
}

// NewRegistry creates a service registry backed by Postgres.
func NewRegistry(db *sql.DB, log *slog.Logger) *Registry {
	podID := os.Getenv("POD_NAME")
	if podID == "" {
		podID = os.Getenv("HOSTNAME")
	}
	if podID == "" {
		podID = "local"
	}
	return &Registry{db: db, log: log, podID: podID}
}

// PodID returns the current pod's identifier.
func (r *Registry) PodID() string {
	return r.podID
}

// Register announces this pod to the registry.
// Creates config keys if they don't exist for this service version.
func (r *Registry) Register(ctx context.Context, serviceName, version, port string) error {
	if r.db == nil {
		return nil
	}

	host, _ := os.Hostname()

	// Ensure table exists
	r.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS service_registry (
			pod_id TEXT NOT NULL,
			service TEXT NOT NULL,
			version TEXT,
			host TEXT,
			port TEXT,
			config_keys JSONB DEFAULT '[]',
			status TEXT DEFAULT 'running',
			started_at TIMESTAMPTZ DEFAULT now(),
			last_ping_at TIMESTAMPTZ DEFAULT now(),
			PRIMARY KEY (service, pod_id)
		)
	`)

	// Get config keys for this service from schema
	var keys []string
	for _, entry := range Schema() {
		if entry.Service == serviceName || entry.Service == "platform" {
			keys = append(keys, entry.Key)
		}
	}
	keysJSON, _ := json.Marshal(keys)

	// Upsert this pod
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO service_registry (pod_id, service, version, host, port, config_keys, status, started_at, last_ping_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'running', now(), now())
		ON CONFLICT (service, pod_id) DO UPDATE SET
			version = $3, host = $4, port = $5, config_keys = $6,
			status = 'running', last_ping_at = now()
	`, r.podID, serviceName, version, host, port, keysJSON)
	if err != nil {
		return err
	}

	// Seed config keys for this service+version if they don't exist
	source := NewPostgresSource(r.db)
	for _, entry := range Schema() {
		if entry.Service != serviceName && entry.Service != "platform" {
			continue
		}
		var exists bool
		r.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM config WHERE key = $1)", entry.Key).Scan(&exists)
		if !exists {
			source.UpdateWithAction(ctx, entry.Key, entry.Default, "config_seed")
			r.log.Debug("seeded config key", "key", entry.Key, "default", entry.Default, "version", version)
		}
	}

	r.log.Info("pod registered",
		"pod_id", r.podID,
		"service", serviceName,
		"version", version,
		"port", port,
		"config_keys", len(keys),
	)
	return nil
}

// Deregister marks this pod as stopped.
func (r *Registry) Deregister(ctx context.Context, serviceName string) error {
	if r.db == nil {
		return nil
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE service_registry SET status = 'stopped', last_ping_at = now()
		WHERE service = $1 AND pod_id = $2
	`, serviceName, r.podID)
	return err
}

// Ping updates the heartbeat timestamp.
func (r *Registry) Ping(ctx context.Context, serviceName string) error {
	if r.db == nil {
		return nil
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE service_registry SET last_ping_at = now()
		WHERE service = $1 AND pod_id = $2
	`, serviceName, r.podID)
	return err
}

// ListPods returns all registered pods.
func (r *Registry) ListPods(ctx context.Context) ([]PodRegistration, error) {
	if r.db == nil {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT pod_id, service, version, host, port, config_keys, status, started_at, last_ping_at
		FROM service_registry ORDER BY service, pod_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []PodRegistration
	for rows.Next() {
		var p PodRegistration
		var keysJSON []byte
		if err := rows.Scan(&p.PodID, &p.Service, &p.Version, &p.Host, &p.Port, &keysJSON, &p.Status, &p.StartedAt, &p.LastPingAt); err != nil {
			continue
		}
		json.Unmarshal(keysJSON, &p.ConfigKeys)
		result = append(result, p)
	}
	return result, nil
}

// ListServices returns pods grouped by service.
func (r *Registry) ListServices(ctx context.Context) ([]ServiceGroup, error) {
	pods, err := r.ListPods(ctx)
	if err != nil {
		return nil, err
	}

	groups := make(map[string]*ServiceGroup)
	for _, p := range pods {
		g, ok := groups[p.Service]
		if !ok {
			g = &ServiceGroup{Service: p.Service, ConfigKeys: p.ConfigKeys}
			groups[p.Service] = g
		}
		g.Pods = append(g.Pods, p)
	}

	var result []ServiceGroup
	for _, g := range groups {
		result = append(result, *g)
	}
	return result, nil
}

// HTTPHandler returns endpoints for the service registry.
func (r *Registry) HTTPHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r2 *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r2.URL.Query().Get("grouped") == "true" {
			groups, err := r.ListServices(r2.Context())
			if err != nil {
				http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
				return
			}
			json.NewEncoder(w).Encode(groups)
			return
		}

		pods, err := r.ListPods(r2.Context())
		if err != nil {
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(pods)
	}
}
