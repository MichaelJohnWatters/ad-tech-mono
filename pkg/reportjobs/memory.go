package reportjobs

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// MemoryJobStore is an in-memory JobStore with the same semantics as the
// Postgres implementation (claim ordering, schedule dedupe, tenant-filtered
// reads). It backs unit tests — per repo convention databases are never
// mocked, they get real in-memory implementations.
type MemoryJobStore struct {
	mu   sync.Mutex
	seq  int
	jobs map[string]*Job
	Now  func() time.Time // defaults to time.Now
}

// NewMemoryJobStore returns an empty MemoryJobStore.
func NewMemoryJobStore() *MemoryJobStore {
	return &MemoryJobStore{jobs: map[string]*Job{}}
}

func (m *MemoryJobStore) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// Enqueue inserts a queued job, mirroring the schedule-dedupe unique index.
func (m *MemoryJobStore) Enqueue(_ context.Context, j Job) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j.Source == SourceSchedule {
		for _, e := range m.jobs {
			if e.SavedReportID == j.SavedReportID && e.Source == SourceSchedule &&
				(e.Status == StatusQueued || e.Status == StatusRunning) {
				return "", nil
			}
		}
	}
	m.seq++
	j.ID = fmt.Sprintf("job-%d", m.seq)
	j.Status = StatusQueued
	j.CreatedAt = m.now()
	if j.ExpiresAt.IsZero() {
		j.ExpiresAt = j.CreatedAt.Add(30 * 24 * time.Hour)
	}
	m.jobs[j.ID] = &j
	return j.ID, nil
}

// ClaimOne claims the oldest queued job.
func (m *MemoryJobStore) ClaimOne(_ context.Context) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var oldest *Job
	for _, j := range m.jobs {
		if j.Status != StatusQueued {
			continue
		}
		if oldest == nil || j.CreatedAt.Before(oldest.CreatedAt) ||
			(j.CreatedAt.Equal(oldest.CreatedAt) && j.ID < oldest.ID) {
			oldest = j
		}
	}
	if oldest == nil {
		return nil, nil
	}
	t := m.now()
	oldest.Status = StatusRunning
	oldest.StartedAt = &t
	oldest.Attempts++
	cp := *oldest
	return &cp, nil
}

// MarkDone records a successful run and its artifact.
func (m *MemoryJobStore) MarkDone(_ context.Context, id string, a Artifact) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return fmt.Errorf("job %s not found", id)
	}
	t := m.now()
	j.Status = StatusDone
	j.FinishedAt = &t
	j.Error = ""
	j.ArtifactBucket, j.ArtifactKey = a.Bucket, a.Key
	j.ArtifactBytes, j.RowCount = a.Bytes, a.Rows
	return nil
}

// MarkFailed records a failed run.
func (m *MemoryJobStore) MarkFailed(_ context.Context, id, errMsg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return fmt.Errorf("job %s not found", id)
	}
	t := m.now()
	j.Status = StatusFailed
	j.FinishedAt = &t
	j.Error = errMsg
	return nil
}

// ListByAccount returns the account's jobs, newest first.
func (m *MemoryJobStore) ListByAccount(_ context.Context, accountID string, limit int) ([]Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 {
		limit = 50
	}
	out := []Job{}
	for _, j := range m.jobs {
		if j.AccountID == accountID {
			out = append(out, *j)
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if !out[a].CreatedAt.Equal(out[b].CreatedAt) {
			return out[a].CreatedAt.After(out[b].CreatedAt)
		}
		return out[a].ID > out[b].ID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// GetByAccount returns one job only if it belongs to the account.
func (m *MemoryJobStore) GetByAccount(_ context.Context, accountID, id string) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok || j.AccountID != accountID {
		return nil, nil
	}
	cp := *j
	return &cp, nil
}

// Expired returns done/failed jobs past their expires_at.
func (m *MemoryJobStore) Expired(_ context.Context, now time.Time, limit int) ([]Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 {
		limit = 100
	}
	out := []Job{}
	for _, j := range m.jobs {
		if (j.Status == StatusDone || j.Status == StatusFailed) && j.ExpiresAt.Before(now) {
			out = append(out, *j)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ExpiresAt.Before(out[b].ExpiresAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Delete removes a job row.
func (m *MemoryJobStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.jobs, id)
	return nil
}

// ExtendLease is a no-op for the in-memory store (single-process tests).
func (m *MemoryJobStore) ExtendLease(_ context.Context, _ string) error { return nil }

// ReclaimExpired flips running jobs back to queued (memory store treats any
// running job as expired — tests drive timing explicitly).
func (m *MemoryJobStore) ReclaimExpired(_ context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, j := range m.jobs {
		if j.Status == StatusRunning {
			j.Status = StatusQueued
			n++
		}
	}
	return n, nil
}
