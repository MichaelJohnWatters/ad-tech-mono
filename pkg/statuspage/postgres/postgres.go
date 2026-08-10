// Package postgres implements statuspage.Store on the platform-global incidents
// table (no tenant scoping / no RLS — incidents are platform-wide).
package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/lib/pq"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/statuspage"
)

// Store is the Postgres-backed incident store.
type Store struct{ db *sql.DB }

// New returns a Store backed by db.
func New(db *sql.DB) *Store { return &Store{db: db} }

const incidentCols = `id::text, title, COALESCE(body,''), impact, status,
       affected_components, started_at, resolved_at, COALESCE(created_by,''), updated_at`

func scanIncident(scan func(dest ...any) error) (statuspage.Incident, error) {
	var inc statuspage.Incident
	var comps pq.StringArray
	var resolved sql.NullTime
	if err := scan(&inc.ID, &inc.Title, &inc.Body, &inc.Impact, &inc.Status,
		&comps, &inc.StartedAt, &resolved, &inc.CreatedBy, &inc.UpdatedAt); err != nil {
		return statuspage.Incident{}, err
	}
	inc.AffectedComponents = comps
	if resolved.Valid {
		inc.ResolvedAt = &resolved.Time
	}
	return inc, nil
}

func (s *Store) query(ctx context.Context, where string, args ...any) ([]statuspage.Incident, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+incidentCols+` FROM incidents `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("query incidents: %w", err)
	}
	defer rows.Close()
	out := []statuspage.Incident{}
	for rows.Next() {
		inc, err := scanIncident(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, inc)
	}
	return out, rows.Err()
}

// RecentIncidents returns open incidents plus resolved ones within the window.
func (s *Store) RecentIncidents(ctx context.Context, window time.Duration, limit int) ([]statuspage.Incident, error) {
	if limit <= 0 {
		limit = 20
	}
	cutoff := time.Now().Add(-window)
	return s.query(ctx,
		`WHERE status <> 'resolved' OR started_at >= $1 ORDER BY started_at DESC LIMIT $2`, cutoff, limit)
}

// ListAll returns incidents newest-first for the staff console.
func (s *Store) ListAll(ctx context.Context, limit int) ([]statuspage.Incident, error) {
	if limit <= 0 {
		limit = 100
	}
	return s.query(ctx, `ORDER BY started_at DESC LIMIT $1`, limit)
}

// Create inserts a new incident.
func (s *Store) Create(ctx context.Context, inc statuspage.Incident) (string, error) {
	var createdBy any
	if inc.CreatedBy != "" {
		createdBy = inc.CreatedBy
	}
	var id string
	err := s.db.QueryRowContext(ctx, `
INSERT INTO incidents (title, body, impact, status, affected_components, created_by)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id::text`,
		inc.Title, inc.Body, inc.Impact, inc.Status, pq.Array(inc.AffectedComponents), createdBy).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("create incident: %w", err)
	}
	return id, nil
}

// Update mutates status/impact/body/components. resolved_at is set the first
// time status becomes 'resolved' and cleared if it reopens.
func (s *Store) Update(ctx context.Context, inc statuspage.Incident) (*statuspage.Incident, error) {
	row := s.db.QueryRowContext(ctx, `
UPDATE incidents SET
    title = $2, body = $3, impact = $4, status = $5, affected_components = $6,
    resolved_at = CASE WHEN $5 = 'resolved' THEN COALESCE(resolved_at, now()) ELSE NULL END,
    updated_at = now()
WHERE id = $1::uuid
RETURNING `+incidentCols,
		inc.ID, inc.Title, inc.Body, inc.Impact, inc.Status, pq.Array(inc.AffectedComponents))
	updated, err := scanIncident(row.Scan)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("update incident: %w", err)
	}
	return &updated, nil
}
