package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/podid"
)

// Manager provides live configuration with polling, change detection,
// and callback notifications. Sits on top of Config and adds:
//   - Configurable poll rate (default 30s)
//   - Change callbacks (notify services when config changes)
//   - HTTP API for reading/updating config
//   - Thread-safe reads during poll updates
//
// Usage:
//
//	mgr := config.NewManager(cfg, logger)
//	mgr.SetPollInterval(10 * time.Second)
//	mgr.OnChange("exchange.bid_timeout", func(key, oldVal, newVal string) {
//	    log.Info("bid timeout changed", "old", oldVal, "new", newVal)
//	})
//	mgr.Start(ctx) // begins polling
type Manager struct {
	mu           sync.RWMutex
	cfg          *Config
	log          *slog.Logger
	pollInterval time.Duration
	callbacks    map[string][]ChangeCallback
	globalCBs    []ChangeCallback
	history      []ChangeRecord
	source       ConfigSource
	registry     *Registry
	serviceName  string
	stopCh       chan struct{}
	// bus, when set, broadcasts adtech.cache.invalidate.config on every Set
	// and triggers a re-poll on receipt. Lets a PUT /v1/config on one pod
	// propagate to every other pod within NATS round-trip time, instead of
	// waiting for the next 30s poll tick. nil = poll-only mode.
	bus        events.EventBus
	subscribed bool // true after Subscribe to invalidate subject succeeds
}

// ChangeCallback is called when a config value changes.
type ChangeCallback func(key, oldValue, newValue string)

// ChangeRecord logs a config change for audit.
type ChangeRecord struct {
	Key       string    `json:"key"`
	OldValue  string    `json:"old_value"`
	NewValue  string    `json:"new_value"`
	Source    string    `json:"source"` // poll, api, nats
	Timestamp time.Time `json:"timestamp"`
}

// ConfigSource provides live config values (Postgres, HTTP, file, etc.)
type ConfigSource interface {
	// FetchAll returns every row in the source — used by admin tooling and
	// the manager UI. Pod read paths use FetchAllForPod for scoped reads.
	FetchAll(ctx context.Context) (map[string]string, error)
	// FetchAllForPod returns the values visible to a specific pod: its
	// own pod-scoped rows merged on top of legacy global rows. Sources
	// without a pod concept (in-memory test source) just return FetchAll.
	FetchAllForPod(ctx context.Context, podID string) (map[string]string, error)
	// Update sets a config value in the backing store.
	Update(ctx context.Context, key, value string) error
	// Delete removes a config value from the backing store.
	Delete(ctx context.Context, key string) error
}

// NewManager creates a config manager.
func NewManager(cfg *Config, log *slog.Logger) *Manager {
	return &Manager{
		cfg:          cfg,
		log:          log,
		pollInterval: 30 * time.Second,
		callbacks:    make(map[string][]ChangeCallback),
		stopCh:       make(chan struct{}),
	}
}

// SetPollInterval changes how often live config is refreshed.
func (m *Manager) SetPollInterval(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pollInterval = d
	m.log.Info("config poll interval changed", "interval", d)
}

// SetRegistry links the registry for heartbeat pings during polling.
func (m *Manager) SetRegistry(registry *Registry, serviceName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.registry = registry
	m.serviceName = serviceName
}

// SetSource sets the backing store for live config.
func (m *Manager) SetSource(source ConfigSource) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.source = source
}

// SetBus wires a NATS-style event bus so Set() can broadcast invalidates
// and Start() can subscribe to re-poll on receipt. Call before Start().
// nil = poll-only mode (unchanged from before this method existed).
func (m *Manager) SetBus(bus events.EventBus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bus = bus
}

// OnChange registers a callback for when a specific key changes.
func (m *Manager) OnChange(key string, cb ChangeCallback) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.callbacks[key] = append(m.callbacks[key], cb)
}

// OnAnyChange registers a callback for any config change.
func (m *Manager) OnAnyChange(cb ChangeCallback) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.globalCBs = append(m.globalCBs, cb)
}

// Start begins polling the config source at the configured interval.
func (m *Manager) Start(ctx context.Context) {
	m.mu.RLock()
	interval := m.pollInterval
	m.mu.RUnlock()

	m.log.Info("config manager started", "poll_interval", interval)

	// Initial fetch + first subscribe attempt. Subscribe may fail if no
	// service has called EnsureStream yet — that's fine, the poll loop
	// retries it each tick.
	m.poll(ctx)
	m.trySubscribeInvalidate(ctx)

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-m.stopCh:
				return
			case <-ticker.C:
				m.poll(ctx)
				m.trySubscribeInvalidate(ctx)
			}
		}
	}()
}

// trySubscribeInvalidate establishes the NATS subscription for
// adtech.cache.invalidate.config. No-op when (a) no bus is wired, or (b)
// already subscribed. Retries every poll tick until success so the order
// of "stream creation by some service" vs "manager Start" doesn't matter.
func (m *Manager) trySubscribeInvalidate(ctx context.Context) {
	m.mu.Lock()
	if m.bus == nil || m.subscribed {
		m.mu.Unlock()
		return
	}
	bus := m.bus
	m.mu.Unlock()

	// Per-REPLICA group so every replica re-polls on a config invalidate.
	// registry.PodID() (POD_NAME) is SHARED across a service's replicas (stable
	// per-pod config identity), so it must NOT be the group suffix — that would
	// queue-group the replicas and only one would re-poll. podid.Replica() is
	// the unique hostname.
	group := "config-invalidate-" + podid.Replica()
	// SubscribeBroadcast: per-pod EPHEMERAL consumer that JetStream reaps when
	// the pod dies. The old per-pod-group Subscribe created a permanent durable
	// per pod name that never got cleaned up — the config-invalidate consumers
	// were a large share of the 1039 orphans that wedged JetStream on
	// 2026-07-25. Falls back to Subscribe on buses without broadcast support.
	err := events.SubscribeBroadcast(ctx, bus, events.SubjectCacheInvalidateConfig, group, func(c context.Context, msg *events.Message) error {
		m.log.Info("config invalidate received, re-polling", "key", string(msg.Data))
		m.poll(c)
		_ = msg.Ack()
		return nil
	})
	if err != nil {
		// Expected on first attempts before any service has ensured the
		// stream. Logged at Debug to avoid spamming Warn every poll tick.
		m.log.Debug("config invalidate subscribe failed (will retry)", "error", err)
		return
	}
	m.mu.Lock()
	m.subscribed = true
	m.mu.Unlock()
	m.log.Info("config invalidate subscribed")
}

// Stop halts polling.
func (m *Manager) Stop() {
	close(m.stopCh)
}

func (m *Manager) poll(ctx context.Context) {
	m.mu.RLock()
	source := m.source
	registry := m.registry
	serviceName := m.serviceName
	m.mu.RUnlock()

	if source == nil {
		return
	}

	// Pod-scoped fetch: the manager polls only the values visible to this
	// pod (own rows + legacy globals). Other pods' overrides stay out of
	// our in-memory map, so reads can't pick up someone else's tuning.
	podID := ""
	if registry != nil {
		podID = registry.PodID()
	}
	values, err := source.FetchAllForPod(ctx, podID)
	if err != nil {
		m.log.Warn("config poll failed", "error", err)
		return
	}

	// The poll result is a COMPLETE snapshot of this pod's live rows, so it
	// is authoritative for the whole live layer — including rows that no
	// longer exist. applySnapshot (not applyChanges) so a deleted row
	// reverts the key to its env/schema-default value instead of pinning
	// the last-known value in memory forever.
	m.applySnapshot(values, "poll")

	if registry != nil && serviceName != "" {
		registry.Ping(ctx, serviceName)
		// Idempotent prune of stale pods (k8s rolling-update remnants,
		// scale-down residue). Runs from every pod's poll loop — the
		// DELETE is cheap and runs only when stale rows actually exist.
		// Threshold defaults to 5 min inside PruneStale (10× the
		// default 30 s poll interval).
		if cfgN, regN, err := registry.PruneStale(ctx, 0); err != nil {
			m.log.Warn("registry prune failed", "error", err)
		} else if cfgN > 0 || regN > 0 {
			m.log.Info("registry pruned", "config_rows", cfgN, "registry_rows", regN)
		}
	}
}

// applyChanges applies a PARTIAL update (a single API write, a rollback).
// It never removes keys — only applySnapshot, fed by a complete poll
// fetch, is allowed to decide that a live value no longer exists.
func (m *Manager) applyChanges(newValues map[string]string, source string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.applyLocked(newValues, source)
}

// applySnapshot applies a COMPLETE fetch of this pod's live rows: present
// values are applied, and any key currently in the live layer but absent
// from the snapshot is cleared so reads revert to env/schema default.
// Without this, deleting a config row pinned the last-known value in every
// running pod until restart — a silent lie ops could not see or fix short
// of writing the old value back by hand.
func (m *Manager) applySnapshot(newValues map[string]string, source string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for key, oldVal := range m.cfg.snapshot() {
		if _, still := newValues[key]; still {
			continue
		}
		m.cfg.ClearLive(key)
		// The effective value after the clear — env var or schema default —
		// is what callbacks receive, so Live* handles rebase to something
		// parseable instead of sticking on the deleted value.
		effective := m.cfg.Get(key, schemaDefault(key))
		m.recordChangeLocked(key, oldVal, effective, source)
		m.log.Info("config row removed, reverting to env/default",
			"key", key, "old", oldVal, "new", effective, "source", source)
	}

	m.applyLocked(newValues, source)
}

// applyLocked is the shared body of applyChanges/applySnapshot. Caller
// holds m.mu.
func (m *Manager) applyLocked(newValues map[string]string, source string) {
	for key, newVal := range newValues {
		oldVal, existed := m.cfg.rawGet(key)
		if !existed || oldVal != newVal {
			m.cfg.SetLive(key, newVal)
			m.recordChangeLocked(key, oldVal, newVal, source)
			m.log.Info("config changed", "key", key, "old", oldVal, "new", newVal, "source", source)
		}
	}
}

// recordChangeLocked appends history and fires callbacks. Caller holds m.mu.
func (m *Manager) recordChangeLocked(key, oldVal, newVal, source string) {
	m.history = append(m.history, ChangeRecord{
		Key: key, OldValue: oldVal, NewValue: newVal,
		Source: source, Timestamp: time.Now(),
	})
	for _, cb := range m.callbacks[key] {
		go cb(key, oldVal, newVal)
	}
	for _, cb := range m.globalCBs {
		go cb(key, oldVal, newVal)
	}
}

// schemaDefault returns the schema default for key, or "" when the key
// isn't in the platform schema (raw keys resolve their default at the
// call site).
func schemaDefault(key string) string {
	for _, e := range Schema() {
		if e.Key == key {
			return e.Default
		}
	}
	return ""
}

// Set updates a config value via the API (bypasses polling).
// Validates the value type if the key is in the schema.
func (m *Manager) Set(ctx context.Context, key, value string) error {
	return m.SetForPod(ctx, "", key, value)
}

// SetForPod writes a value scoped to a specific pod (empty podID = the
// legacy global row). Same flow as Set: validate, persist, replay through
// applyChanges so OnChange callbacks fire for any pod whose in-memory
// view matches.
func (m *Manager) SetForPod(ctx context.Context, podID, key, value string) error {
	if err := m.validateForPod(ctx, podID, key, value); err != nil {
		return err
	}

	m.mu.RLock()
	source := m.source
	m.mu.RUnlock()

	if source != nil {
		if pg, ok := source.(*PostgresSource); ok {
			if err := pg.UpdateForPod(ctx, podID, key, value, "config_update"); err != nil {
				return err
			}
		} else if err := source.Update(ctx, key, value); err != nil {
			return err
		}
	}

	// Only echo the change into the in-memory map if it's our own pod or
	// the global scope — other pods will pick it up on their next poll
	// (or NATS invalidate).
	ownPod := ""
	if m.registry != nil {
		ownPod = m.registry.PodID()
	}
	if podID == "" || podID == ownPod {
		m.applyChanges(map[string]string{key: value}, "api")
	}

	// Broadcast so sibling pods re-poll immediately. Best-effort — if NATS
	// is unreachable the change still lands eventually via the 30s poll,
	// so we log but don't fail the write.
	m.mu.RLock()
	bus := m.bus
	m.mu.RUnlock()
	if bus != nil {
		if err := bus.Publish(ctx, events.SubjectCacheInvalidateConfig, []byte(key)); err != nil {
			m.log.Warn("config invalidate publish failed", "key", key, "error", err)
		} else {
			m.log.Info("config invalidate published", "key", key)
		}
	}
	return nil
}

// validateForPod runs schema type validation. Falls through to the
// pod-owned schema (service_registry.schema_entries) for keys that
// aren't in the platform Schema() — DSP/exchange/tracker etc. each
// own their config keys, so dsp.daily_budget_default has its `int`
// type only in dsp-internal-0's schema row. Without this, the
// platform Validate() returned nil ("unknown key, allowed") and an
// int-typed key would accept arbitrary strings.
func (m *Manager) validateForPod(ctx context.Context, podID, key, value string) error {
	for _, entry := range Schema() {
		if entry.Key == key {
			return validateType(entry, value)
		}
	}
	if m.registry == nil || podID == "" {
		return nil
	}
	pods, err := m.registry.ListPods(ctx)
	if err != nil {
		return nil
	}
	for _, p := range pods {
		if p.PodID != podID {
			continue
		}
		for _, entry := range p.SchemaEntries {
			if entry.Key == key {
				return validateType(entry, value)
			}
		}
	}
	return nil
}

// Rollback reverts a config key to its previous value.
// Checks persistent audit log (Postgres) first, falls back to in-memory history.
func (m *Manager) Rollback(ctx context.Context, key string) error {
	m.mu.RLock()
	source := m.source
	m.mu.RUnlock()

	var previousValue string
	found := false

	// Try persistent history from Postgres audit log first
	if pgSource, ok := source.(*PostgresSource); ok {
		history, err := pgSource.History(ctx, key, 2)
		if err == nil && len(history) >= 1 {
			// history[0] is the most recent change, its OldValue is what we want
			previousValue = history[0].OldValue
			found = true
		}
	}

	// Fall back to in-memory history
	if !found {
		m.mu.RLock()
		for i := len(m.history) - 1; i >= 0; i-- {
			if m.history[i].Key == key {
				previousValue = m.history[i].OldValue
				found = true
				break
			}
		}
		m.mu.RUnlock()
	}

	if !found {
		return fmt.Errorf("no history for key %q", key)
	}

	if previousValue == "" {
		return m.Remove(ctx, key)
	}

	// Write the rollback to Postgres with action type
	if source != nil {
		if pgSource, ok := source.(*PostgresSource); ok {
			if err := pgSource.UpdateWithAction(ctx, key, previousValue, "config_rollback"); err != nil {
				return err
			}
		} else {
			if err := source.Update(ctx, key, previousValue); err != nil {
				return err
			}
		}
	}
	m.applyChanges(map[string]string{key: previousValue}, "rollback")
	return nil
}

// ResetToDefault resets a config key back to its schema default value.
func (m *Manager) ResetToDefault(ctx context.Context, key string) error {
	for _, entry := range Schema() {
		if entry.Key == key {
			if entry.Default == "" {
				return m.Remove(ctx, key)
			}
			m.mu.RLock()
			source := m.source
			m.mu.RUnlock()
			if source != nil {
				if pgSource, ok := source.(*PostgresSource); ok {
					if err := pgSource.UpdateWithAction(ctx, key, entry.Default, "config_reset"); err != nil {
						return err
					}
				} else {
					if err := source.Update(ctx, key, entry.Default); err != nil {
						return err
					}
				}
			}
			m.applyChanges(map[string]string{key: entry.Default}, "reset")
			return nil
		}
	}
	return fmt.Errorf("key %q not in schema", key)
}

// Remove deletes a config value. The key reverts to its env/schema-default
// value locally, callbacks fire with that effective value (so Live* handles
// rebase), and an invalidate is broadcast so sibling pods re-poll and drop
// the row too instead of waiting for their next poll tick.
func (m *Manager) Remove(ctx context.Context, key string) error {
	m.mu.RLock()
	source := m.source
	m.mu.RUnlock()

	if source != nil {
		if err := source.Delete(ctx, key); err != nil {
			return err
		}
	}

	m.mu.Lock()
	oldVal, _ := m.cfg.rawGet(key)
	m.cfg.ClearLive(key)
	effective := m.cfg.Get(key, schemaDefault(key))
	m.recordChangeLocked(key, oldVal, effective, "api")
	m.mu.Unlock()

	m.log.Info("config removed, reverting to env/default", "key", key, "old", oldVal, "new", effective)

	m.mu.RLock()
	bus := m.bus
	m.mu.RUnlock()
	if bus != nil {
		if err := bus.Publish(ctx, events.SubjectCacheInvalidateConfig, []byte(key)); err != nil {
			m.log.Warn("config invalidate publish failed", "key", key, "error", err)
		}
	}
	return nil
}

// History returns recent config changes.
func (m *Manager) History(limit int) []ChangeRecord {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if limit <= 0 || limit > len(m.history) {
		limit = len(m.history)
	}
	start := len(m.history) - limit
	result := make([]ChangeRecord, limit)
	copy(result, m.history[start:])
	return result
}

// All returns all current live config values.
func (m *Manager) All() map[string]string {
	return m.cfg.snapshot()
}

// HTTPHandler returns an HTTP handler for the config management API.
//
//	GET  /config         - list all config
//	GET  /config?key=x   - get single key
//	PUT  /config         - set a key {"key": "x", "value": "y"}
//	DELETE /config?key=x - delete a key
//	GET  /config/history - change history
func (m *Manager) HTTPHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.Method {
		case http.MethodGet:
			if r.URL.Query().Get("resolved") == "true" {
				key := r.URL.Query().Get("key")
				pod := r.URL.Query().Get("pod") // optional, defaults to this pod
				if pod == "" && m.registry != nil {
					pod = m.registry.PodID()
				}
				resolved := m.resolveSource(r.Context(), key, pod)
				json.NewEncoder(w).Encode(resolved)
				return
			}
			if r.URL.Query().Get("history") == "true" {
				// Audit log from Postgres. Per-key when ?key=X is set, or
				// the platform-wide tail (last N changes across all keys)
				// when only ?history=true is set. Limit defaults to 50/200
				// to keep the page lightweight.
				key := r.URL.Query().Get("key")
				limit := 50
				if key == "" {
					limit = 200
				}
				if pgSource, ok := m.source.(*PostgresSource); ok {
					history, err := pgSource.History(r.Context(), key, limit)
					if err == nil {
						json.NewEncoder(w).Encode(history)
						return
					}
				}
				// In-memory fallback for when the source isn't Postgres.
				json.NewEncoder(w).Encode(m.History(limit))
				return
			}
			if r.URL.Query().Get("schema") == "true" {
				// Show schema with current values + source provenance per key.
				// The UI uses Source/SourcePod to render the per-row "where
				// did this value come from" badge.
				//
				// Schema source: union of every running pod's published
				// schema_entries (read from service_registry). Fallback to
				// the in-process registry when no Registry is wired (tests,
				// or when Postgres is unreachable at boot).
				var schema []SchemaEntry
				if m.registry != nil {
					if entries, err := m.registry.AllSchemas(r.Context()); err == nil && len(entries) > 0 {
						schema = entries
					}
				}
				if schema == nil {
					schema = Schema()
				}
				type entry struct {
					SchemaEntry
					CurrentValue string `json:"current_value"`
					Status       string `json:"status"` // active, orphaned, new
					Source       string `json:"source,omitempty"`
					SourcePod    string `json:"source_pod,omitempty"`
				}

				// Source resolution is per-pod. Default to the requesting
				// service's own pod; UI can override with ?pod=X to view a
				// specific pod's provenance.
				pod := r.URL.Query().Get("pod")
				if pod == "" && m.registry != nil {
					pod = m.registry.PodID()
				}

				schemaKeys := make(map[string]bool)
				var result []entry
				for _, s := range schema {
					schemaKeys[s.Key] = true
					e := entry{
						SchemaEntry:  s,
						CurrentValue: m.cfg.Get(s.Key, s.Default),
						Status:       "active",
					}
					// Secret rows: redact value, mark source unconditionally
					// as env (or default if the env var isn't set).
					if s.Tier == TierSecret {
						e.CurrentValue = "***"
					}
					rc := m.resolveSource(r.Context(), s.Key, pod)
					e.Source = rc.Source
					e.SourcePod = rc.PodID
					result = append(result, e)
				}

				// Add orphaned keys from Postgres that aren't in current schema
				allValues := m.All()
				for key, val := range allValues {
					if !schemaKeys[key] {
						result = append(result, entry{
							SchemaEntry: SchemaEntry{
								Key:         key,
								Type:        "string",
								Default:     "",
								Description: "Not in current schema (orphaned from previous version)",
								Service:     "unknown",
								Deprecated:  true,
							},
							CurrentValue: val,
							Status:       "orphaned",
						})
					}
				}

				// Filter by service if requested
				svc := r.URL.Query().Get("service")
				if svc != "" {
					var filtered []entry
					for _, e := range result {
						if e.Service == svc {
							filtered = append(filtered, e)
						}
					}
					result = filtered
				}
				json.NewEncoder(w).Encode(result)
				return
			}
			if r.URL.Query().Get("rollback") != "" {
				key := r.URL.Query().Get("rollback")
				if err := m.Rollback(r.Context(), key); err != nil {
					http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
					return
				}
				json.NewEncoder(w).Encode(map[string]string{"key": key, "status": "rolled_back"})
				return
			}
			if r.URL.Query().Get("reset") != "" {
				key := r.URL.Query().Get("reset")
				if err := m.ResetToDefault(r.Context(), key); err != nil {
					http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
					return
				}
				json.NewEncoder(w).Encode(map[string]string{"key": key, "status": "reset_to_default"})
				return
			}
			key := r.URL.Query().Get("key")
			if key != "" {
				val := m.cfg.Get(key, "")
				json.NewEncoder(w).Encode(map[string]string{"key": key, "value": val})
			} else {
				json.NewEncoder(w).Encode(m.All())
			}

		case http.MethodPut:
			var req struct {
				Key   string `json:"key"`
				Value string `json:"value"`
				PodID string `json:"pod_id"` // empty = global, otherwise scoped to that pod
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
				return
			}
			if err := m.SetForPod(r.Context(), req.PodID, req.Key, req.Value); err != nil {
				status := http.StatusInternalServerError
				// Schema validation errors are client-input problems,
				// not server failures. Return 400 so callers can
				// distinguish them from a real persistence outage.
				if errors.Is(err, ErrValidation) {
					status = http.StatusBadRequest
				}
				http.Error(w, `{"error":"`+err.Error()+`"}`, status)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"key": req.Key, "value": req.Value, "status": "updated"})

		case http.MethodDelete:
			key := r.URL.Query().Get("key")
			if key == "" {
				http.Error(w, `{"error":"key required"}`, http.StatusBadRequest)
				return
			}
			if err := m.Remove(r.Context(), key); err != nil {
				http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"key": key, "status": "deleted"})

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

// ResolvedConfig is the response shape of the /v1/config?resolved=true
// endpoint. It tells the UI (and ops tooling) exactly where a value came
// from — pod row, legacy global row, env var, or schema default. Powers
// the Source column in the config manager UI so operators stop having to
// read code to answer "why is this value X right now?".
type ResolvedConfig struct {
	Key       string `json:"key"`
	Tier      string `json:"tier"`     // live / static / secret (from schema)
	Value     string `json:"value"`    // effective resolved value
	Source    string `json:"source"`   // postgres-pod / postgres-global / env / default / unknown
	PodID     string `json:"pod_id"`   // populated when Source = postgres-pod
	EnvKey    string `json:"env_key"`  // env var name checked (always reported)
	Default   string `json:"default"`  // schema default, for comparison
	Redacted  bool   `json:"redacted"` // true for secret tier (value masked)
}

// resolveSource computes the provenance for a key + pod. The order
// mirrors Config.Get's read path + the upstream FetchAllForPod merge
// rules, but reaches into the source for the pod_id of the winning row.
func (m *Manager) resolveSource(ctx context.Context, key, podID string) ResolvedConfig {
	out := ResolvedConfig{Key: key, PodID: podID, EnvKey: envKeyFromConfigKey(key)}

	// Pull schema metadata (tier + default).
	for _, e := range Schema() {
		if e.Key == key {
			out.Tier = e.Tier
			out.Default = e.Default
			break
		}
	}

	// Secret tier: never look in Postgres; env-only. Mask the value.
	if out.Tier == TierSecret {
		v := os.Getenv(out.EnvKey)
		if v == "" {
			v = out.Default
			out.Source = "default"
		} else {
			out.Source = "env"
		}
		out.Value = "***"
		out.Redacted = true
		_ = v
		return out
	}

	// Live + Static: Postgres for pod, then global, then env, then default.
	// Only Live should actually have Postgres rows; if Static has one it's
	// stale data from before the tier model (flag via Source value).
	if pg, ok := m.source.(*PostgresSource); ok && podID != "" {
		var valueJSON []byte
		err := pg.db.QueryRowContext(ctx,
			"SELECT value FROM config WHERE pod_id = $1 AND key = $2",
			podID, key,
		).Scan(&valueJSON)
		if err == nil && len(valueJSON) > 0 {
			out.Value = decodeJSONStringOrRaw(valueJSON)
			out.Source = "postgres-pod"
			return out
		}
	}
	if pg, ok := m.source.(*PostgresSource); ok {
		var valueJSON []byte
		err := pg.db.QueryRowContext(ctx,
			"SELECT value FROM config WHERE pod_id = '' AND key = $1",
			key,
		).Scan(&valueJSON)
		if err == nil && len(valueJSON) > 0 {
			out.Value = decodeJSONStringOrRaw(valueJSON)
			out.Source = "postgres-global"
			return out
		}
	}
	if v := os.Getenv(out.EnvKey); v != "" {
		out.Value = v
		out.Source = "env"
		return out
	}
	out.Value = out.Default
	out.Source = "default"
	return out
}

// MemorySource is an in-memory config source for testing/local dev.
type MemorySource struct {
	mu     sync.RWMutex
	values map[string]string
}

// NewMemorySource creates an in-memory config source with initial values.
func NewMemorySource(initial map[string]string) *MemorySource {
	values := make(map[string]string)
	for k, v := range initial {
		values[k] = v
	}
	return &MemorySource{values: values}
}

func (s *MemorySource) FetchAll(_ context.Context) (map[string]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.values))
	for k, v := range s.values {
		out[k] = v
	}
	return out, nil
}

// FetchAllForPod returns the same set for in-memory tests — there's no
// pod scoping for this source.
func (s *MemorySource) FetchAllForPod(ctx context.Context, _ string) (map[string]string, error) {
	return s.FetchAll(ctx)
}

func (s *MemorySource) Update(_ context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[key] = value
	return nil
}

func (s *MemorySource) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, key)
	return nil
}
