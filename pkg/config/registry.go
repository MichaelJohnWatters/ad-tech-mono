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
// SchemaEntries is the pod's full owned schema (service-specific +
// platform-shared keys) — the config-manager UI unions these across every
// running pod to render the key table.
type PodRegistration struct {
	PodID          string        `json:"pod_id"`
	Service        string        `json:"service"`
	Version        string        `json:"version"`
	Host           string        `json:"host"`
	Port           string        `json:"port"`
	SchemaEntries  []SchemaEntry `json:"schema_entries"`
	Status         string        `json:"status"` // running, stopped
	StartedAt      time.Time     `json:"started_at"`
	LastPingAt     time.Time     `json:"last_ping_at"`
}

// ServiceGroup is a service with all its running pods.
type ServiceGroup struct {
	Service       string            `json:"service"`
	Pods          []PodRegistration `json:"pods"`
	SchemaEntries []SchemaEntry     `json:"schema_entries"`
}

// Registry tracks which pods are running, what version they are, and the
// schema of keys they own. The pod is the source of truth for its own
// schema — there is no separate config_schema table anymore.
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

// Register announces this pod to the registry. The full schema slice (this
// service's owned keys + the platform-shared defaults) is written into
// service_registry.schema_entries so the config-manager UI can render the
// pod's keys + types + descriptions without a separate publication step.
// Live-tier keys are seeded into the per-pod config table on first boot.
func (r *Registry) Register(ctx context.Context, serviceName, version, port string, schema []SchemaEntry) error {
	if r.db == nil {
		return nil
	}

	host, _ := os.Hostname()

	// service_registry holds the pod's identity + its full schema. Created
	// here (not in a migration) so unit tests with a fresh DB don't need
	// the migration runner.
	r.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS service_registry (
			pod_id TEXT NOT NULL,
			service TEXT NOT NULL,
			version TEXT,
			host TEXT,
			port TEXT,
			schema_entries JSONB NOT NULL DEFAULT '[]',
			status TEXT DEFAULT 'running',
			started_at TIMESTAMPTZ DEFAULT now(),
			last_ping_at TIMESTAMPTZ DEFAULT now(),
			PRIMARY KEY (service, pod_id)
		)
	`)

	schemaJSON, _ := json.Marshal(schema)

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO service_registry (pod_id, service, version, host, port, schema_entries, status, started_at, last_ping_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'running', now(), now())
		ON CONFLICT (service, pod_id) DO UPDATE SET
			version = $3, host = $4, port = $5, schema_entries = $6,
			status = 'running', last_ping_at = now()
	`, r.podID, serviceName, version, host, port, schemaJSON)
	if err != nil {
		return err
	}

	// Seed live-tier keys for *this pod* if absent. ON CONFLICT preserves
	// any operator tuning across restarts — a row only gets the default
	// when no prior value exists for this (pod_id, key).
	source := NewPostgresSource(r.db)
	liveCount := 0
	for _, entry := range schema {
		if entry.Tier != TierLive {
			continue
		}
		liveCount++
		var exists bool
		r.db.QueryRowContext(ctx,
			"SELECT EXISTS(SELECT 1 FROM config WHERE pod_id = $1 AND key = $2)",
			r.podID, entry.Key,
		).Scan(&exists)
		if exists {
			continue
		}
		if err := source.UpdateForPod(ctx, r.podID, entry.Key, entry.Default, "config_seed"); err != nil {
			r.log.Warn("seed failed", "key", entry.Key, "pod", r.podID, "error", err)
			continue
		}
		r.log.Debug("seeded config key", "key", entry.Key, "pod", r.podID, "default", entry.Default, "version", version)
	}

	r.log.Info("pod registered",
		"pod_id", r.podID,
		"service", serviceName,
		"version", version,
		"port", port,
		"schema_keys", len(schema),
		"live_keys", liveCount,
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

// PruneStale removes registry rows + per-pod config rows for pods
// whose last heartbeat is older than threshold. Each pod's config
// Manager calls this from its poll tick so dead k8s pods (rolling
// updates, scale events) don't leave forever-growing config rows
// behind, and the config-manager UI's pod list stays accurate.
//
// Threshold defaults to 5 minutes if zero — that's 10× the default
// 30 s poll interval, so a healthy pod will ping 10 times within
// the window, generously absorbing transient network blips.
//
// Returns (configRowsDeleted, registryRowsDeleted). Errors don't
// fail the poll loop; the caller logs and moves on.
func (r *Registry) PruneStale(ctx context.Context, threshold time.Duration) (int64, int64, error) {
	if r.db == nil {
		return 0, 0, nil
	}
	if threshold == 0 {
		threshold = 5 * time.Minute
	}
	// Step 1: identify stale pod_ids. Use a CTE so both DELETEs see
	// the same set even if a pod heartbeats between the two queries.
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	// Find stale pod_ids inside the transaction. Both stopped pods
	// (explicit Deregister) and silently-dead ones (heartbeat stalled)
	// are caught — the unifying signal is last_ping_at.
	stalePods, err := tx.QueryContext(ctx, `
		SELECT pod_id FROM service_registry
		WHERE last_ping_at < now() - ($1::text || ' seconds')::interval
	`, int(threshold.Seconds()))
	if err != nil {
		return 0, 0, err
	}
	var ids []string
	for stalePods.Next() {
		var id string
		if err := stalePods.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	stalePods.Close()
	if len(ids) == 0 {
		return 0, 0, tx.Commit()
	}

	// Delete config rows for stale pod_ids. lib/pq lets us pass a
	// string slice as a uuid/text array via pq.Array, but we already
	// have it in plain []string and the simplest way to use it is via
	// ANY($1::text[]).
	cfgRes, err := tx.ExecContext(ctx, `
		DELETE FROM config WHERE pod_id = ANY($1::text[])
	`, "{"+joinIDs(ids)+"}")
	if err != nil {
		return 0, 0, err
	}
	cfgN, _ := cfgRes.RowsAffected()

	regRes, err := tx.ExecContext(ctx, `
		DELETE FROM service_registry WHERE pod_id = ANY($1::text[])
	`, "{"+joinIDs(ids)+"}")
	if err != nil {
		return 0, 0, err
	}
	regN, _ := regRes.RowsAffected()

	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return cfgN, regN, nil
}

// joinIDs builds the comma-separated body of a Postgres text array
// literal. Safe because pod_ids are platform-generated identifiers
// (k8s pod names + a few hardcoded service-N suffixes), not operator
// input — no embedded quotes or commas to escape.
func joinIDs(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	out := ids[0]
	for _, id := range ids[1:] {
		out += "," + id
	}
	return out
}

// ListPods returns all registered pods, each with its full schema slice.
func (r *Registry) ListPods(ctx context.Context) ([]PodRegistration, error) {
	if r.db == nil {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT pod_id, service, version, host, port, schema_entries, status, started_at, last_ping_at
		FROM service_registry ORDER BY service, pod_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []PodRegistration
	for rows.Next() {
		var p PodRegistration
		var schemaJSON []byte
		if err := rows.Scan(&p.PodID, &p.Service, &p.Version, &p.Host, &p.Port, &schemaJSON, &p.Status, &p.StartedAt, &p.LastPingAt); err != nil {
			continue
		}
		_ = json.Unmarshal(schemaJSON, &p.SchemaEntries)
		result = append(result, p)
	}
	return result, nil
}

// ListServices returns pods grouped by service. Each group's SchemaEntries
// is the union of its pods' schemas (pods of the same service share a
// schema, but unioning is robust if a rolling deploy mixes versions).
func (r *Registry) ListServices(ctx context.Context) ([]ServiceGroup, error) {
	pods, err := r.ListPods(ctx)
	if err != nil {
		return nil, err
	}

	groups := make(map[string]*ServiceGroup)
	for _, p := range pods {
		g, ok := groups[p.Service]
		if !ok {
			g = &ServiceGroup{Service: p.Service}
			groups[p.Service] = g
		}
		g.Pods = append(g.Pods, p)
	}

	for _, g := range groups {
		seen := map[string]bool{}
		for _, p := range g.Pods {
			for _, e := range p.SchemaEntries {
				if seen[e.Key] {
					continue
				}
				seen[e.Key] = true
				g.SchemaEntries = append(g.SchemaEntries, e)
			}
		}
	}

	var result []ServiceGroup
	for _, g := range groups {
		result = append(result, *g)
	}
	return result, nil
}

// AllSchemas returns the union of every running pod's schema. Used by the
// config-manager UI to render the full key set without a separate
// config_schema table.
func (r *Registry) AllSchemas(ctx context.Context) ([]SchemaEntry, error) {
	pods, err := r.ListPods(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []SchemaEntry
	for _, p := range pods {
		for _, e := range p.SchemaEntries {
			if seen[e.Key] {
				continue
			}
			seen[e.Key] = true
			out = append(out, e)
		}
	}
	return out, nil
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
