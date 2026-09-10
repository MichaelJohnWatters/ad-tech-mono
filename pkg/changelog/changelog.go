// Package changelog is the API changelog (PLAN Phase 11 #109): a public,
// staff-authored log of API changes for external integrators. Platform-global
// (no tenant scoping / no RLS — staff write, everyone reads), mirroring the
// status-page/incidents shape (pkg/statuspage).
package changelog

import (
	"context"
	"time"
)

// Valid categories (Keep-a-Changelog style).
var validCategories = map[string]bool{
	"added": true, "changed": true, "deprecated": true,
	"removed": true, "fixed": true, "security": true,
}

// ValidCategory reports whether c is a known changelog category.
func ValidCategory(c string) bool { return validCategories[c] }

// Entry is one published API-changelog item.
type Entry struct {
	ID                string    `json:"id"`
	Version           string    `json:"version"`
	ReleaseDate       time.Time `json:"release_date"`
	Category          string    `json:"category"`
	Breaking          bool      `json:"breaking"`
	Title             string    `json:"title"`
	Body              string    `json:"body"`
	AffectedEndpoints []string  `json:"affected_endpoints"`
	// CreatedBy (staff JWT subject) is never serialized to the public feed.
	CreatedBy string    `json:"-"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Input is the staff-supplied payload for creating/updating an entry.
type Input struct {
	Version           string    `json:"version"`
	ReleaseDate       time.Time `json:"release_date"`
	Category          string    `json:"category"`
	Breaking          bool      `json:"breaking"`
	Title             string    `json:"title"`
	Body              string    `json:"body"`
	AffectedEndpoints []string  `json:"affected_endpoints"`
}

// Store is the changelog persistence surface. Platform-global — no account scoping.
type Store interface {
	// Recent returns published entries newest-first (release_date desc), capped.
	Recent(ctx context.Context, limit int) ([]Entry, error)
	// Create inserts an entry authored by createdBy (JWT subject); returns its id.
	Create(ctx context.Context, in Input, createdBy string) (string, error)
	// Update mutates an existing entry by id.
	Update(ctx context.Context, id string, in Input) error
	// Delete removes an entry by id.
	Delete(ctx context.Context, id string) error
}
