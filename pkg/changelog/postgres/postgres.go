// Package postgres implements changelog.Store on the platform-global
// changelog_entries table (no tenant scoping / no RLS — changelog is platform-wide,
// staff-authored; mirrors pkg/statuspage/postgres).
package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/lib/pq"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/changelog"
)

// Store is the Postgres-backed changelog store.
type Store struct{ db *sql.DB }

// New returns a Store backed by db.
func New(db *sql.DB) *Store { return &Store{db: db} }

const cols = `id::text, version, release_date, category, breaking, title,
       COALESCE(body,''), affected_endpoints, COALESCE(created_by,''), created_at, updated_at`

func scanEntry(scan func(dest ...any) error) (changelog.Entry, error) {
	var e changelog.Entry
	var endpoints pq.StringArray
	if err := scan(&e.ID, &e.Version, &e.ReleaseDate, &e.Category, &e.Breaking,
		&e.Title, &e.Body, &endpoints, &e.CreatedBy, &e.CreatedAt, &e.UpdatedAt); err != nil {
		return changelog.Entry{}, err
	}
	e.AffectedEndpoints = endpoints
	return e, nil
}

// Recent returns published entries newest-first, capped at limit.
func (s *Store) Recent(ctx context.Context, limit int) ([]changelog.Entry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+cols+` FROM changelog_entries ORDER BY release_date DESC, created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("changelog recent: %w", err)
	}
	defer rows.Close()
	out := []changelog.Entry{}
	for rows.Next() {
		e, err := scanEntry(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Create inserts an entry, returning its id.
func (s *Store) Create(ctx context.Context, in changelog.Input, createdBy string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO changelog_entries (version, release_date, category, breaking, title, body, affected_endpoints, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id::text`,
		in.Version, in.ReleaseDate, in.Category, in.Breaking, in.Title, in.Body,
		pq.Array(in.AffectedEndpoints), createdBy).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("changelog create: %w", err)
	}
	return id, nil
}

// Update mutates an existing entry by id.
func (s *Store) Update(ctx context.Context, id string, in changelog.Input) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE changelog_entries
		 SET version=$2, release_date=$3, category=$4, breaking=$5, title=$6, body=$7,
		     affected_endpoints=$8, updated_at=now()
		 WHERE id=$1::uuid`,
		id, in.Version, in.ReleaseDate, in.Category, in.Breaking, in.Title, in.Body, pq.Array(in.AffectedEndpoints))
	if err != nil {
		return fmt.Errorf("changelog update: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// Delete removes an entry by id.
func (s *Store) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM changelog_entries WHERE id=$1::uuid`, id)
	if err != nil {
		return fmt.Errorf("changelog delete: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
