package audiencemappings

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// MemoryStore is an in-memory Store with the same tenant-scoping + name-upsert
// semantics as the Postgres implementation. It backs unit tests — per repo
// convention databases are never mocked, they get real in-memory
// implementations.
type MemoryStore struct {
	mu       sync.Mutex
	seq      int
	mappings map[string]*Mapping
	Now      func() time.Time // defaults to time.Now
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{mappings: map[string]*Mapping{}}
}

func (m *MemoryStore) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// Create inserts or (on account+name conflict) updates the account's mapping.
func (m *MemoryStore) Create(_ context.Context, mp Mapping) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if mp.IDType == "" {
		mp.IDType = "user_id"
	}
	// Upsert on (account_id, name).
	for _, e := range m.mappings {
		if e.AccountID == mp.AccountID && e.Name == mp.Name {
			e.Mappings = mp.Mappings
			e.IDType = mp.IDType
			e.UpdatedAt = now
			return e.ID, nil
		}
	}
	m.seq++
	mp.ID = fmt.Sprintf("mapping-%d", m.seq)
	mp.CreatedAt = now
	mp.UpdatedAt = now
	cp := mp
	m.mappings[mp.ID] = &cp
	return mp.ID, nil
}

// ListByAccount returns the account's mappings, newest first.
func (m *MemoryStore) ListByAccount(_ context.Context, accountID string) ([]Mapping, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Mapping{}
	for _, e := range m.mappings {
		if e.AccountID == accountID {
			out = append(out, *e)
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if !out[a].CreatedAt.Equal(out[b].CreatedAt) {
			return out[a].CreatedAt.After(out[b].CreatedAt)
		}
		return out[a].ID > out[b].ID
	})
	return out, nil
}

// GetByAccount returns one mapping only if it belongs to the account.
func (m *MemoryStore) GetByAccount(_ context.Context, accountID, id string) (*Mapping, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.mappings[id]
	if !ok || e.AccountID != accountID {
		return nil, nil
	}
	cp := *e
	return &cp, nil
}

// DeleteByAccount removes one mapping scoped to the account.
func (m *MemoryStore) DeleteByAccount(_ context.Context, accountID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.mappings[id]; ok && e.AccountID == accountID {
		delete(m.mappings, id)
	}
	return nil
}
