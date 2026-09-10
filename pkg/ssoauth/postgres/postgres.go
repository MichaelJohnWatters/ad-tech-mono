// Package postgres implements ssoauth.Store on the sso_configurations table
// (RLS: owner tenant-scoped; the pre-auth login path reads via the platform hatch).
package postgres

import (
	"context"
	"database/sql"

	"github.com/lib/pq"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ssoauth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

// Store is the Postgres-backed SSO config store.
type Store struct {
	db    *sql.DB
	store *postgres.Store
}

// New returns a Store backed by db.
func New(db *sql.DB) *Store { return &Store{db: db, store: postgres.NewFromDB(db)} }

const cols = `account_id::text, enabled, issuer, client_id, client_secret, allowed_domains, default_role`

// ByAccount reads the account's config via the platform hatch (the login path runs
// pre-auth, with no tenant context) + an explicit account_id filter.
func (s *Store) ByAccount(ctx context.Context, accountID string) (ssoauth.Config, bool, error) {
	var c ssoauth.Config
	var domains pq.StringArray
	err := s.store.QueryRowPlatform(ctx, func(row *sql.Row) error {
		return row.Scan(&c.AccountID, &c.Enabled, &c.Issuer, &c.ClientID, &c.ClientSecret, &domains, &c.DefaultRole)
	}, `SELECT `+cols+` FROM sso_configurations WHERE account_id = $1::uuid`, accountID)
	if err == sql.ErrNoRows {
		return ssoauth.Config{}, false, nil
	}
	if err != nil {
		return ssoauth.Config{}, false, err
	}
	c.AllowedDomains = domains
	return c, true, nil
}

// Upsert writes the account's config, scoped by the tenant GUC so RLS admits it.
func (s *Store) Upsert(ctx context.Context, cfg ssoauth.Config) error {
	_, err := postgres.ExecTenantDB(ctx, s.db, cfg.AccountID,
		`INSERT INTO sso_configurations (account_id, enabled, issuer, client_id, client_secret, allowed_domains, default_role, updated_at)
		 VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, now())
		 ON CONFLICT (account_id) DO UPDATE SET
		   enabled=$2, issuer=$3, client_id=$4,
		   client_secret=CASE WHEN $5 = '' THEN sso_configurations.client_secret ELSE $5 END,
		   allowed_domains=$6, default_role=$7, updated_at=now()`,
		cfg.AccountID, cfg.Enabled, cfg.Issuer, cfg.ClientID, cfg.ClientSecret, pq.Array(cfg.AllowedDomains), cfg.DefaultRole)
	return err
}
