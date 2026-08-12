// Package postgres implements partner.Store on the platform-global `partners`
// table (no tenant scoping / no RLS — the partner registry is platform-wide,
// staff-managed, like incidents). Writes run as the app role directly.
package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/lib/pq"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/partner"
)

// ErrNotFound is returned when a partner id doesn't exist. ErrDuplicateName /
// ErrConstraint let the handler turn a client-caused DB violation into a 4xx
// instead of a 500.
var (
	ErrNotFound           = errors.New("partner: not found")
	ErrDuplicateName      = errors.New("partner: duplicate name")
	ErrConstraint         = errors.New("partner: constraint violation")
	ErrAlreadyProvisioned = errors.New("partner: login already provisioned")
)

// classify maps a Postgres error to a client-facing sentinel where the cause is
// a client input (unique/check violation), else returns it unchanged.
func classify(err error) error {
	var pe *pq.Error
	if errors.As(err, &pe) {
		switch pe.Code {
		case "23505": // unique_violation
			return ErrDuplicateName
		case "23514", "22P02": // check_violation, invalid_text_representation (bad ::uuid etc.)
			return ErrConstraint
		}
	}
	return err
}

// Store is the Postgres-backed partner registry.
type Store struct{ db *sql.DB }

// New returns a Store backed by db.
func New(db *sql.DB) *Store { return &Store{db: db} }

const partnerCols = `id::text, name, kind, status, endpoint_bid, endpoint_nurl, seat,
       auth_method, auth_secret_ref, channels, formats, timeout_ms, max_qps,
       openrtb_version, contact_tech, contact_billing, notes,
       COALESCE(created_by,''), created_at, updated_at, onboarded_at,
       COALESCE(account_id::text,'')`

func scanPartner(scan func(dest ...any) error) (partner.Partner, error) {
	var p partner.Partner
	var channels, formats pq.StringArray
	var maxQPS sql.NullInt64
	var onboarded sql.NullTime
	if err := scan(&p.ID, &p.Name, &p.Kind, &p.Status, &p.EndpointBid, &p.EndpointNURL, &p.Seat,
		&p.AuthMethod, &p.AuthSecretRef, &channels, &formats, &p.TimeoutMs, &maxQPS,
		&p.OpenRTBVersion, &p.ContactTech, &p.ContactBilling, &p.Notes,
		&p.CreatedBy, &p.CreatedAt, &p.UpdatedAt, &onboarded, &p.AccountID); err != nil {
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
		if c := classify(err); c != err {
			return partner.Partner{}, c
		}
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

// Update edits mutable metadata (never status — that's SetStatus). It is a PATCH,
// not a replace: a field left empty/zero (or an omitted max_qps / empty array)
// PRESERVES the stored value rather than clobbering it. Callers (the staff portal
// form, or a partial API edit) send only a subset of fields — without this,
// editing one field would reset auth_method/openrtb_version to defaults and wipe
// contact_billing/notes/max_qps. (Trade-off: a field can't be blanked back to
// empty via edit — rare for a staff registry.) `name` is always set (the handler
// requires it non-empty).
func (s *Store) Update(ctx context.Context, id string, in partner.Input) (partner.Partner, error) {
	if s.db == nil {
		return partner.Partner{}, sql.ErrConnDone
	}
	var maxQPS any
	if in.MaxQPS != nil {
		maxQPS = *in.MaxQPS
	}
	row := s.db.QueryRowContext(ctx, `
UPDATE partners
   SET name            = $2,
       kind            = COALESCE(NULLIF($3,''), kind),
       endpoint_bid    = COALESCE(NULLIF($4,''), endpoint_bid),
       endpoint_nurl   = COALESCE(NULLIF($5,''), endpoint_nurl),
       seat            = COALESCE(NULLIF($6,''), seat),
       auth_method     = COALESCE(NULLIF($7,''), auth_method),
       channels        = CASE WHEN cardinality($8::text[]) > 0 THEN $8 ELSE channels END,
       formats         = CASE WHEN cardinality($9::text[]) > 0 THEN $9 ELSE formats END,
       timeout_ms      = COALESCE(NULLIF($10,0), timeout_ms),
       max_qps         = COALESCE($11, max_qps),
       openrtb_version = COALESCE(NULLIF($12,''), openrtb_version),
       contact_tech    = COALESCE(NULLIF($13,''), contact_tech),
       contact_billing = COALESCE(NULLIF($14,''), contact_billing),
       notes           = COALESCE(NULLIF($15,''), notes),
       updated_at      = now()
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
		if c := classify(err); c != err {
			return partner.Partner{}, c
		}
		return partner.Partner{}, fmt.Errorf("update partner: %w", err)
	}
	return p, nil
}

// ProvisionLogin creates the partner's login account (type 'partner') + owner
// user and links partners.account_id — one transaction. Mirrors signup's
// CreateAccountWithOwner (the accounts insert is admitted for the app role; the
// tenant GUC is set before the team_members insert so RLS admits it).
func (s *Store) ProvisionLogin(ctx context.Context, partnerID, name, email, passwordHash string) (string, error) {
	if s.db == nil {
		return "", sql.ErrConnDone
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	var existing sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT account_id::text FROM partners WHERE id=$1::uuid`, partnerID).Scan(&existing)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if existing.Valid && existing.String != "" {
		return "", ErrAlreadyProvisioned
	}

	var accountID string
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO accounts (name, email, type, status, created_at, updated_at)
		 VALUES ($1, $2, 'partner', 'active', now(), now()) RETURNING id::text`,
		name, email).Scan(&accountID); err != nil {
		return "", classify(err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, accountID); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO team_members (account_id, email, name, role, password_hash, status, created_at, updated_at)
		 VALUES ($1, $2, $3, 'owner', $4, 'active', now(), now())`,
		accountID, email, name, passwordHash); err != nil {
		return "", classify(err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE partners SET account_id = $2::uuid, updated_at = now() WHERE id = $1::uuid`,
		partnerID, accountID); err != nil {
		return "", err
	}
	return accountID, tx.Commit()
}

// GetByAccount returns the registry row a provisioned partner login owns.
func (s *Store) GetByAccount(ctx context.Context, accountID string) (partner.Partner, error) {
	if s.db == nil {
		return partner.Partner{}, sql.ErrConnDone
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+partnerCols+` FROM partners WHERE account_id = $1::uuid`, accountID)
	p, err := scanPartner(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return partner.Partner{}, ErrNotFound
	}
	if err != nil {
		return partner.Partner{}, fmt.Errorf("get partner by account: %w", err)
	}
	return p, nil
}

// RecordCertification persists a certification run.
func (s *Store) RecordCertification(ctx context.Context, partnerID string, res partner.CertificationResult, runBy string) (partner.CertificationRecord, error) {
	if s.db == nil {
		return partner.CertificationRecord{}, sql.ErrConnDone
	}
	checks, err := json.Marshal(res.Checks)
	if err != nil {
		return partner.CertificationRecord{}, err
	}
	var rec partner.CertificationRecord
	var raw []byte
	if err := s.db.QueryRowContext(ctx, `
INSERT INTO partner_certifications (partner_id, passed, score, total, checks, run_by)
VALUES ($1::uuid, $2, $3, $4, $5::jsonb, $6)
RETURNING id::text, passed, score, total, checks, COALESCE(run_by,''), created_at`,
		partnerID, res.Passed, res.Score, res.Total, string(checks), nullStr(runBy)).
		Scan(&rec.ID, &rec.Passed, &rec.Score, &rec.Total, &raw, &rec.RunBy, &rec.CreatedAt); err != nil {
		if c := classify(err); c != err {
			return partner.CertificationRecord{}, c
		}
		return partner.CertificationRecord{}, fmt.Errorf("record certification: %w", err)
	}
	rec.Checks = raw
	return rec, nil
}

// ListCertifications returns a partner's runs, newest first.
func (s *Store) ListCertifications(ctx context.Context, partnerID string, limit int) ([]partner.CertificationRecord, error) {
	if s.db == nil {
		return nil, sql.ErrConnDone
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id::text, passed, score, total, checks, COALESCE(run_by,''), created_at
FROM partner_certifications WHERE partner_id = $1::uuid
ORDER BY created_at DESC LIMIT $2`, partnerID, limit)
	if err != nil {
		return nil, fmt.Errorf("list certifications: %w", err)
	}
	defer rows.Close()
	out := []partner.CertificationRecord{}
	for rows.Next() {
		var rec partner.CertificationRecord
		var raw []byte
		if err := rows.Scan(&rec.ID, &rec.Passed, &rec.Score, &rec.Total, &raw, &rec.RunBy, &rec.CreatedAt); err != nil {
			return nil, err
		}
		rec.Checks = raw
		out = append(out, rec)
	}
	return out, rows.Err()
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
