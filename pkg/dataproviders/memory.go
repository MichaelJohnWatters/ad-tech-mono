package dataproviders

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
	mu        sync.Mutex
	seq       int
	providers map[string]*Provider
	Now       func() time.Time // defaults to time.Now
}

// NewMemoryStore returns an empty in-memory provider store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{providers: map[string]*Provider{}}
}

func (s *MemoryStore) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Create inserts or upserts by (account_id, name), returning the id.
func (s *MemoryStore) Create(_ context.Context, p Provider) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	// Upsert on (account, name).
	for id, existing := range s.providers {
		if existing.AccountID == p.AccountID && existing.Name == p.Name {
			p.ID = id
			p.CreatedAt = existing.CreatedAt
			p.UpdatedAt = now
			cp := p
			s.providers[id] = &cp
			return id, nil
		}
	}
	s.seq++
	id := fmt.Sprintf("provider-%d", s.seq)
	p.ID = id
	p.CreatedAt = now
	p.UpdatedAt = now
	cp := p
	s.providers[id] = &cp
	return id, nil
}

// ListByAccount returns the account's providers, newest first.
func (s *MemoryStore) ListByAccount(_ context.Context, accountID string) ([]Provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Provider{}
	for _, p := range s.providers {
		if p.AccountID == accountID {
			out = append(out, *p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// GetByAccount returns one provider only if it belongs to the account.
func (s *MemoryStore) GetByAccount(_ context.Context, accountID, id string) (*Provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.providers[id]
	if !ok || p.AccountID != accountID {
		return nil, nil
	}
	cp := *p
	return &cp, nil
}

// DeleteByAccount removes one provider scoped to the account.
func (s *MemoryStore) DeleteByAccount(_ context.Context, accountID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.providers[id]; ok && p.AccountID == accountID {
		delete(s.providers, id)
	}
	return nil
}
