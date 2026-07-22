package ingestjobs

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// MemoryIngestStore is an in-memory Store with the same semantics as the
// Postgres implementation (due-gated claim ordering, file dedupe, lease
// reclaim, tenant-filtered reads). It backs unit tests and the pipeline's
// memory fallback — per repo convention databases are never mocked, they get
// real in-memory implementations.
type MemoryIngestStore struct {
	mu   sync.Mutex
	seq  int
	jobs map[string]*Job
	// leases tracks when a running job's lease expires, so ReclaimExpired can
	// distinguish a live claimant from a dead one (mirrors lease_expires_at).
	leases map[string]time.Time
	Now    func() time.Time // defaults to time.Now
}

// NewMemoryIngestStore returns an empty MemoryIngestStore.
func NewMemoryIngestStore() *MemoryIngestStore {
	return &MemoryIngestStore{jobs: map[string]*Job{}, leases: map[string]time.Time{}}
}

func (m *MemoryIngestStore) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// Enqueue inserts a queued job, mirroring the (file_bucket, file_key) dedupe
// unique index over queued/running rows.
func (m *MemoryIngestStore) Enqueue(_ context.Context, j Job) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.jobs {
		if e.FileBucket == j.FileBucket && e.FileKey == j.FileKey &&
			(e.Status == StatusQueued || e.Status == StatusRunning) {
			return "", nil
		}
	}
	m.seq++
	j.ID = fmt.Sprintf("ingest-%d", m.seq)
	j.Status = StatusQueued
	j.CreatedAt = m.now()
	if j.RunAt.IsZero() {
		j.RunAt = j.CreatedAt
	}
	if j.MaxAttempts <= 0 {
		j.MaxAttempts = 5
	}
	m.jobs[j.ID] = &j
	return j.ID, nil
}

// ClaimOne claims the oldest DUE queued job (run_at <= now), by run_at then
// created_at then id.
func (m *MemoryIngestStore) ClaimOne(_ context.Context) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	var oldest *Job
	for _, j := range m.jobs {
		if j.Status != StatusQueued || j.RunAt.After(now) {
			continue
		}
		if oldest == nil || less(j, oldest) {
			oldest = j
		}
	}
	if oldest == nil {
		return nil, nil
	}
	oldest.Status = StatusRunning
	oldest.StartedAt = &now
	oldest.Attempts++
	m.leases[oldest.ID] = now.Add(LeaseTTL)
	cp := *oldest
	return &cp, nil
}

// ClaimByID claims a specific queued job by id (ignores run_at — the caller
// decided it is due). Returns (nil, nil) when absent or not queued.
func (m *MemoryIngestStore) ClaimByID(_ context.Context, id string) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok || j.Status != StatusQueued {
		return nil, nil
	}
	now := m.now()
	j.Status = StatusRunning
	j.StartedAt = &now
	j.Attempts++
	m.leases[id] = now.Add(LeaseTTL)
	cp := *j
	return &cp, nil
}

// less orders by run_at, then created_at, then id.
func less(a, b *Job) bool {
	if !a.RunAt.Equal(b.RunAt) {
		return a.RunAt.Before(b.RunAt)
	}
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.Before(b.CreatedAt)
	}
	return a.ID < b.ID
}

// ExtendLease heartbeats a running job's lease.
func (m *MemoryIngestStore) ExtendLease(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j, ok := m.jobs[id]; ok && j.Status == StatusRunning {
		m.leases[id] = m.now().Add(LeaseTTL)
	}
	return nil
}

// MarkDone records a successful run and its result counts.
func (m *MemoryIngestStore) MarkDone(_ context.Context, id string, r IngestResult) error {
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
	j.SegmentID = r.SegmentID
	j.TotalRows, j.ValidRows, j.RejectedRows = r.TotalRows, r.ValidRows, r.RejectedRows
	j.MatchedRows, j.MatchRate, j.RejectedKey = r.MatchedRows, r.MatchRate, r.RejectedKey
	delete(m.leases, id)
	return nil
}

// MarkFailed records a failed run.
func (m *MemoryIngestStore) MarkFailed(_ context.Context, id, errMsg string) error {
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
	delete(m.leases, id)
	return nil
}

// ListByAccount returns the account's jobs, newest first.
func (m *MemoryIngestStore) ListByAccount(_ context.Context, accountID string, limit int) ([]Job, error) {
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
func (m *MemoryIngestStore) GetByAccount(_ context.Context, accountID, id string) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok || j.AccountID != accountID {
		return nil, nil
	}
	cp := *j
	return &cp, nil
}

// ReclaimExpired flips running jobs whose lease lapsed back to queued.
func (m *MemoryIngestStore) ReclaimExpired(_ context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	n := 0
	for id, j := range m.jobs {
		if j.Status != StatusRunning {
			continue
		}
		exp, ok := m.leases[id]
		if !ok || exp.Before(now) {
			j.Status = StatusQueued
			j.StartedAt = nil
			delete(m.leases, id)
			n++
		}
	}
	return n, nil
}
