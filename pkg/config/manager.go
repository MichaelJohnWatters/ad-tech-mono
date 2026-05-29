package config

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
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
	stopCh       chan struct{}
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
	// FetchAll returns all live config key-value pairs.
	FetchAll(ctx context.Context) (map[string]string, error)
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

// SetSource sets the backing store for live config.
func (m *Manager) SetSource(source ConfigSource) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.source = source
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

	// Initial fetch
	m.poll(ctx)

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
			}
		}
	}()
}

// Stop halts polling.
func (m *Manager) Stop() {
	close(m.stopCh)
}

func (m *Manager) poll(ctx context.Context) {
	m.mu.RLock()
	source := m.source
	m.mu.RUnlock()

	if source == nil {
		return
	}

	values, err := source.FetchAll(ctx)
	if err != nil {
		m.log.Warn("config poll failed", "error", err)
		return
	}

	m.applyChanges(values, "poll")
}

func (m *Manager) applyChanges(newValues map[string]string, source string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for key, newVal := range newValues {
		oldVal, existed := m.cfg.values[key]
		if !existed || oldVal != newVal {
			m.cfg.values[key] = newVal

			record := ChangeRecord{
				Key: key, OldValue: oldVal, NewValue: newVal,
				Source: source, Timestamp: time.Now(),
			}
			m.history = append(m.history, record)

			m.log.Info("config changed", "key", key, "old", oldVal, "new", newVal, "source", source)

			// Fire key-specific callbacks
			for _, cb := range m.callbacks[key] {
				go cb(key, oldVal, newVal)
			}
			// Fire global callbacks
			for _, cb := range m.globalCBs {
				go cb(key, oldVal, newVal)
			}
		}
	}
}

// Set updates a config value via the API (bypasses polling).
// Validates the value type if the key is in the schema.
func (m *Manager) Set(ctx context.Context, key, value string) error {
	if err := Validate(key, value); err != nil {
		return err
	}

	m.mu.RLock()
	source := m.source
	m.mu.RUnlock()

	if source != nil {
		if err := source.Update(ctx, key, value); err != nil {
			return err
		}
	}

	m.applyChanges(map[string]string{key: value}, "api")
	return nil
}

// Rollback reverts a config key to its previous value using the change history.
func (m *Manager) Rollback(ctx context.Context, key string) error {
	m.mu.RLock()
	var previousValue string
	found := false
	for i := len(m.history) - 1; i >= 0; i-- {
		if m.history[i].Key == key {
			previousValue = m.history[i].OldValue
			found = true
			break
		}
	}
	m.mu.RUnlock()

	if !found {
		return fmt.Errorf("no history for key %q", key)
	}

	if previousValue == "" {
		return m.Remove(ctx, key)
	}

	// Bypass validation for rollback (restoring known-good value)
	m.mu.RLock()
	source := m.source
	m.mu.RUnlock()
	if source != nil {
		if err := source.Update(ctx, key, previousValue); err != nil {
			return err
		}
	}
	m.applyChanges(map[string]string{key: previousValue}, "rollback")
	return nil
}

// Remove deletes a config value.
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
	oldVal := m.cfg.values[key]
	delete(m.cfg.values, key)
	m.history = append(m.history, ChangeRecord{
		Key: key, OldValue: oldVal, NewValue: "",
		Source: "api", Timestamp: time.Now(),
	})
	m.mu.Unlock()

	m.log.Info("config removed", "key", key)
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
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]string, len(m.cfg.values))
	for k, v := range m.cfg.values {
		out[k] = v
	}
	return out
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
			if r.URL.Query().Get("history") == "true" {
				json.NewEncoder(w).Encode(m.History(50))
				return
			}
			if r.URL.Query().Get("schema") == "true" {
				// Show schema with current values
				schema := Schema()
				type entry struct {
					SchemaEntry
					CurrentValue string `json:"current_value"`
				}
				var result []entry
				for _, s := range schema {
					result = append(result, entry{
						SchemaEntry:  s,
						CurrentValue: m.cfg.Get(s.Key, s.Default),
					})
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
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
				return
			}
			if err := m.Set(r.Context(), req.Key, req.Value); err != nil {
				http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
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
