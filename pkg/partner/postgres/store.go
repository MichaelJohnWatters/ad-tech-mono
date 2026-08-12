// Package postgres implements partner.Store on the platform-global `partners`
// table (no tenant scoping / no RLS — the partner registry is platform-wide,
// staff-managed, like incidents). Writes run as the app role directly.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/lib/pq"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/partner"
)

// ErrNotFound is returned when a partner id doesn't exist.
var ErrNotFound = errors.New("partner: not found")

// Store is the Postgres-backed partner registry.
type Store struct{ db *sql.DB }

// New returns a Store backed by db.
func New(db *sql.DB) *Store { return &Store{db: db} }

const partnerCols = `id::text, name, kind, status, endpoint_bid, endpoint_nurl, seat,
       auth_method, auth_secret_ref, channels, formats, timeout_ms, max_qps,
       openrtb_version, contact_tech, contact_billing, notes,
       COALESCE(created_by,''), created_at, updated_at, onboarded_at`

func scanPartner(scan func(dest ...any) error) (partner.Partner, error) {
	var p partner.Partner
	var channels, formats pq.StringArray
	var maxQPS sql.NullInt64
	var onboarded sql.NullTime
	if err := scan(&p.ID, &p.Name, &p.Kind, &p.Status, &p.EndpointBid, &p.EndpointNURL, &p.Seat,
		&p.AuthMethod, &p.AuthSecretRef, &channels, &formats, &p.TimeoutMs, &maxQPS,
		&p.OpenRTBVersion, &p.ContactTech, &p.ContactBilling, &p.Notes,
		&p.CreatedBy, &p.CreatedAt, &p.UpdatedAt, &onboarded); err != nil {
		return partner.Partner{}, err
	}
	p.Channels = channels
	p.Formats = formats
	if p.Channels == nil {
		p.Channels = []string{}
	}
	if p.Formats == nil {
		p.Formats = []string{}
	}
	if maxQPS.Valid {
		v := int(maxQPS.Int64)
		p.MaxQPS = &v
	}
	if onboarded.Valid {
		p.OnboardedAt = &onboarded.Time
	}
	return p, nil
}

// defaulted normalises an Input's optional fields before an insert/update.
func defaulted(in partner.Input) partner.Input {
	if in.Kind == "" {
		in.Kind = partner.KindDSP
	}
	if in.AuthMethod == "" {
		in.AuthMethod = "api_key"
	}
	if in.TimeoutMs <= 0 {
		in.TimeoutMs = 100
	}
	if in.OpenRTBVersion == "" {
		in.OpenRTBVersion = "2.5"
	}
	if in.Channels == nil {
		in.Channels = []string{}
	}
	if in.Formats == nil {
		in.Formats = []string{}
	}
	return in
}

// Create registers a new partner (status pending).
func (s *Store) Create(ctx context.Context, in partner.Input, createdBy string) (partner.Partner, error) {
	if s.db == nil {
		return partner.Partner{}, sql.ErrConnDone
	}
	in = defaulted(in)
	var maxQPS any
	if in.MaxQPS != nil {
		maxQPS = *in.MaxQPS
	}
	row := s.db.QueryRowContext(ctx, `
INSERT INTO partners (name, kind, endpoint_bid, endpoint_nurl, seat, auth_method,
       channels, formats, timeout_ms, max_qps, openrtb_version, contact_tech,
       contact_billing, notes, created_by)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
RETURNING `+partnerCols,
		in.Name, in.Kind, in.EndpointBid, in.EndpointNURL, in.Seat, in.AuthMethod,
		pq.Array(in.Channels), pq.Array(in.Formats), in.TimeoutMs, maxQPS, in.OpenRTBVersion,
		in.ContactTech, in.ContactBilling, in.Notes, nullStr(createdBy))
	p, err := scanPartner(row.Scan)
	if err != nil {
		return partner.Partner{}, fmt.Errorf("create partner: %w", err)
	}
	return p, nil
}

// List returns all partners newest-first, optionally filtered by status.
func (s *Store) List(ctx context.Context, status string) ([]partner.Partner, error) {
	if s.db == nil {
		return nil, sql.ErrConnDone
	}
	where, args := "", []any{}
	if status != "" {
		where = "WHERE status = $1"
		args = append(args, status)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+partnerCols+` FROM partners `+where+` ORDER BY created_at DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("list partners: %w", err)
	}
	defer rows.Close()
	out := []partner.Partner{}
	for rows.Next() {
		p, err := scanPartner(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Get returns one partner by id.
func (s *Store) Get(ctx context.Context, id string) (partner.Partner, error) {
	if s.db == nil {
		return partner.Partner{}, sql.ErrConnDone
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+partnerCols+` FROM partners WHERE id = $1::uuid`, id)
	p, err := scanPartner(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return partner.Partner{}, ErrNotFound
	}
	if err != nil {
		return partner.Partner{}, fmt.Errorf("get partner: %w", err)
	}
	return p, nil
}

// SetStatus transitions the partner's lifecycle status. onboarded_at is stamped
// the first time it becomes active (COALESCE keeps the original on later flips).
func (s *Store) SetStatus(ctx context.Context, id, status string) (partner.Partner, error) {
	if s.db == nil {
		return partner.Partner{}, sql.ErrConnDone
	}
	row := s.db.QueryRowContext(ctx, `
UPDATE partners
   SET status = $2,
       updated_at = now(),
       onboarded_at = CASE WHEN $2 = 'active' THEN COALESCE(onboarded_at, now()) ELSE onboarded_at END
 WHERE id = $1::uuid
RETURNING `+partnerCols, id, status)
	p, err := scanPartner(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return partner.Partner{}, ErrNotFound
	}
	if err != nil {
		return partner.Partner{}, fmt.Errorf("set partner status: %w", err)
	}
	return p, nil
}

// Update edits mutable metadata (never status — that's SetStatus).
func (s *Store) Update(ctx context.Context, id string, in partner.Input) (partner.Partner, error) {
	if s.db == nil {
		return partner.Partner{}, sql.ErrConnDone
	}
	in = defaulted(in)
	var maxQPS any
	if in.MaxQPS != nil {
		maxQPS = *in.MaxQPS
	}
	row := s.db.QueryRowContext(ctx, `
UPDATE partners
   SET name=$2, kind=$3, endpoint_bid=$4, endpoint_nurl=$5, seat=$6, auth_method=$7,
       channels=$8, formats=$9, timeout_ms=$10, max_qps=$11, openrtb_version=$12,
       contact_tech=$13, contact_billing=$14, notes=$15, updated_at=now()
 WHERE id=$1::uuid
RETURNING `+partnerCols,
		id, in.Name, in.Kind, in.EndpointBid, in.EndpointNURL, in.Seat, in.AuthMethod,
		pq.Array(in.Channels), pq.Array(in.Formats), in.TimeoutMs, maxQPS, in.OpenRTBVersion,
		in.ContactTech, in.ContactBilling, in.Notes)
	p, err := scanPartner(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return partner.Partner{}, ErrNotFound
	}
	if err != nil {
		return partner.Partner{}, fmt.Errorf("update partner: %w", err)
	}
	return p, nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
