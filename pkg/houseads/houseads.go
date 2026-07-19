// Package houseads is the platform's own fallback-creative store + picker.
//
// A "house ad" is a creative the platform serves on a no-bid when the publisher
// ad server's stub_on_nobid switch is on — the platform advertising its own
// business instead of an empty no-fill. Staff define house ads per format
// (display / video / native / audio) via the gateway; the publisher ad server
// holds them in a warm cache and asks the Picker for one when a format no-bids.
//
// House ads are PLATFORM-GLOBAL, not tenant-scoped (see migration 053): there is
// no account that owns them, so no account_id filtering applies here. Access
// control is enforced at the gateway (staff-only), not in this store.
package houseads

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Valid formats a house ad can target. Mirrors the publisher ad server's
// serving paths (display/native → HTML markup; video/audio → inline VAST XML).
const (
	FormatDisplay = "display"
	FormatVideo   = "video"
	FormatNative  = "native"
	FormatAudio   = "audio"
)

// ValidFormat reports whether f is one of the four supported formats.
func ValidFormat(f string) bool {
	switch f {
	case FormatDisplay, FormatVideo, FormatNative, FormatAudio:
		return true
	}
	return false
}

// HouseAd is one platform fallback creative. Markup is HTML for display/native
// and inline VAST XML for video/audio; the serving path decides how to render
// it. Weight is the relative selection weight among enabled ads of the format.
type HouseAd struct {
	ID         string    `json:"id"`
	Format     string    `json:"format"`
	Name       string    `json:"name"`
	Markup     string    `json:"markup"`
	LandingURL string    `json:"landing_url,omitempty"`
	Enabled    bool      `json:"enabled"`
	Weight     int       `json:"weight"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Input is the mutable payload for Create / Update. ID/timestamps are managed
// by the store.
type Input struct {
	Format     string `json:"format"`
	Name       string `json:"name"`
	Markup     string `json:"markup"`
	LandingURL string `json:"landing_url,omitempty"`
	Enabled    bool   `json:"enabled"`
	Weight     int    `json:"weight"`
}

// Store is the Postgres-backed house-ad CRUD. All queries are parameterised.
// No tenant scoping — house_ads is platform-global (staff-gated at the gateway).
type Store struct{ db *sql.DB }

// NewStore wraps a *sql.DB.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// List returns every house ad, newest first. Used by the staff editor and the
// ad server's warm-cache loader (LoadAll).
func (s *Store) List(ctx context.Context) ([]HouseAd, error) {
	if s.db == nil {
		return nil, sql.ErrConnDone
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id::text, format, name, markup, COALESCE(landing_url, ''), enabled, weight, created_at, updated_at
		 FROM house_ads ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("query house_ads: %w", err)
	}
	defer rows.Close()
	out := []HouseAd{}
	for rows.Next() {
		var h HouseAd
		if err := rows.Scan(&h.ID, &h.Format, &h.Name, &h.Markup, &h.LandingURL, &h.Enabled, &h.Weight, &h.CreatedAt, &h.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan house_ad: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// Get returns one house ad by id; sql.ErrNoRows if unknown.
func (s *Store) Get(ctx context.Context, id string) (HouseAd, error) {
	if s.db == nil {
		return HouseAd{}, sql.ErrConnDone
	}
	var h HouseAd
	err := s.db.QueryRowContext(ctx,
		`SELECT id::text, format, name, markup, COALESCE(landing_url, ''), enabled, weight, created_at, updated_at
		 FROM house_ads WHERE id = $1::uuid`, id).
		Scan(&h.ID, &h.Format, &h.Name, &h.Markup, &h.LandingURL, &h.Enabled, &h.Weight, &h.CreatedAt, &h.UpdatedAt)
	if err != nil {
		return HouseAd{}, err
	}
	return h, nil
}

// Create inserts a new house ad and returns its generated id.
func (s *Store) Create(ctx context.Context, in Input) (string, error) {
	if s.db == nil {
		return "", sql.ErrConnDone
	}
	weight := in.Weight
	if weight <= 0 {
		weight = 1
	}
	var id string
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO house_ads (format, name, markup, landing_url, enabled, weight, created_at, updated_at)
		 VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, now(), now())
		 RETURNING id::text`,
		in.Format, in.Name, in.Markup, in.LandingURL, in.Enabled, weight).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("insert house_ad: %w", err)
	}
	return id, nil
}

// Update replaces the mutable fields of one house ad; sql.ErrNoRows if unknown.
func (s *Store) Update(ctx context.Context, id string, in Input) error {
	if s.db == nil {
		return sql.ErrConnDone
	}
	weight := in.Weight
	if weight <= 0 {
		weight = 1
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE house_ads
		    SET format = $2, name = $3, markup = $4, landing_url = NULLIF($5, ''),
		        enabled = $6, weight = $7, updated_at = now()
		  WHERE id = $1::uuid`,
		id, in.Format, in.Name, in.Markup, in.LandingURL, in.Enabled, weight)
	if err != nil {
		return fmt.Errorf("update house_ad: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// Delete removes one house ad; sql.ErrNoRows if unknown.
func (s *Store) Delete(ctx context.Context, id string) error {
	if s.db == nil {
		return sql.ErrConnDone
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM house_ads WHERE id = $1::uuid`, id)
	if err != nil {
		return fmt.Errorf("delete house_ad: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
