// Package postgres provides the PostgreSQL data access layer with
// built-in multi-tenancy via Row-Level Security.
//
// Every query method reads account_id from context and sets the
// Postgres session variable before executing. Even if code forgets
// a WHERE clause, RLS blocks cross-tenant access.
//
// Usage:
//
//	store, err := postgres.New(cfg)
//	ctx := postgres.WithAccountID(ctx, "account-uuid")
//	campaigns, err := store.ListLineItems(ctx, filter)
package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	_ "github.com/lib/pq"
)

type contextKey string

const accountIDKey contextKey = "account_id"

// Config holds database connection configuration.
type Config struct {
	// PrimaryURL is the read-write connection (PgBouncer -> primary).
	PrimaryURL string
	// ReadURL is the read-only connection (PgBouncer -> standby).
	// Falls back to PrimaryURL if empty.
	ReadURL string
	// MaxOpenConns is the maximum number of open connections.
	MaxOpenConns int
	// MaxIdleConns is the maximum number of idle connections.
	MaxIdleConns int
	// ConnMaxLifetime is the maximum time a connection can be reused.
	ConnMaxLifetime time.Duration
}

// DefaultConfig returns sensible defaults for local development.
func DefaultConfig() Config {
	return Config{
		PrimaryURL:      routes.DefaultPostgresURL,
		ReadURL:         "", // same as primary in local
		MaxOpenConns:    10,
		MaxIdleConns:    5,
		ConnMaxLifetime: 5 * time.Minute,
	}
}

// Store provides data access to PostgreSQL with multi-tenancy.
type Store struct {
	primary *sql.DB
	read    *sql.DB
}

// NewFromDB wraps an already-open *sql.DB as a Store, using it for both the
// primary and read handles. For callers (e.g. the gateway) that already hold a
// single pooled connection and just need the Store's query methods.
func NewFromDB(db *sql.DB) *Store {
	return &Store{primary: db, read: db}
}

// New creates a Store with primary (read-write) and read (read-only) connections.
func New(cfg Config) (*Store, error) {
	primary, err := openDB(cfg.PrimaryURL, cfg)
	if err != nil {
		return nil, fmt.Errorf("open primary db: %w", err)
	}

	readURL := cfg.ReadURL
	if readURL == "" {
		readURL = cfg.PrimaryURL
	}

	read, err := openDB(readURL, cfg)
	if err != nil {
		primary.Close()
		return nil, fmt.Errorf("open read db: %w", err)
	}

	// Verify RLS is actually enforced for this connection's role. RLS is the
	// multi-tenant safety net (see package doc), but a SUPERUSER / BYPASSRLS role
	// silently ignores every tenant_isolation policy — so this is the one control
	// that, if misconfigured per-environment, disables all row-level isolation
	// without any other symptom. Log loudly; don't fail boot (some callers may
	// legitimately be an admin path). The migrate/seed/admin superuser
	// connections use raw sql.Open, not New, so they won't trip this.
	LogRLSEnforcement(context.Background(), primary, slog.Default())

	return &Store{primary: primary, read: read}, nil
}

// RLSEnforced reports whether the current database role actually has Row-Level
// Security enforced against it. A role that is SUPERUSER or carries BYPASSRLS
// ignores every tenant_isolation policy, so multi-tenant isolation is only as
// strong as this returning true. App services must connect as the least-
// privilege NOBYPASSRLS role (adtech_app, migration 067); only the migrate job
// and the gateway's dev admin URL may use the owning superuser.
func RLSEnforced(ctx context.Context, db *sql.DB) (enforced bool, role string, err error) {
	var super, bypass bool
	err = db.QueryRowContext(ctx,
		`SELECT current_user, rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).
		Scan(&role, &super, &bypass)
	if err != nil {
		return false, "", err
	}
	return !super && !bypass, role, nil
}

// LogRLSEnforcement checks the connected role and logs the result: an info line
// when RLS is enforced, a loud ERROR (the security-misconfiguration signal) when
// the role bypasses it. Never fails the caller — surfacing the mistake is the
// point. Call it once, at boot, on any pool that carries tenant-scoped data.
func LogRLSEnforcement(ctx context.Context, db *sql.DB, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	enforced, role, err := RLSEnforced(ctx, db)
	if err != nil {
		log.Warn("could not verify RLS enforcement for db role", "error", err)
		return
	}
	if enforced {
		log.Info("db role enforces row-level security", "role", role)
		return
	}
	log.Error("SECURITY: db role BYPASSES row-level security — multi-tenant isolation is DISABLED on this connection; connect as the NOBYPASSRLS app role (adtech_app)",
		"role", role)
}

func openDB(url string, cfg Config) (*sql.DB, error) {
	db, err := sql.Open("postgres", url)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping failed: %w", err)
	}
	return db, nil
}

// Close closes both primary and read database connections.
func (s *Store) Close() error {
	s.primary.Close()
	s.read.Close()
	return nil
}

// Primary returns the read-write database for explicit use.
func (s *Store) Primary() *sql.DB {
	return s.primary
}

// Read returns the read-only database for explicit use.
func (s *Store) Read() *sql.DB {
	return s.read
}

// WithAccountID adds the tenant account_id to the context.
// All store methods use this to enforce multi-tenancy.
func WithAccountID(ctx context.Context, accountID string) context.Context {
	return context.WithValue(ctx, accountIDKey, accountID)
}

// AccountIDFromContext extracts the account_id from context.
// Returns empty string if not set (platform-level queries).
func AccountIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(accountIDKey).(string); ok {
		return v
	}
	return ""
}

// SetTenantContext sets the Postgres session variable for RLS.
// Must be called at the start of every tenant-scoped query.
// Uses SET LOCAL so it's scoped to the current transaction.
func SetTenantContext(ctx context.Context, tx *sql.Tx) error {
	accountID := AccountIDFromContext(ctx)
	if accountID == "" {
		return fmt.Errorf("account_id not set in context")
	}
	_, err := tx.ExecContext(ctx, "SELECT set_config('app.current_account_id', $1, true)", accountID)
	return err
}

// WithTx executes a function within a transaction with tenant context set.
// This is the standard pattern for all tenant-scoped writes.
func (s *Store) WithTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.primary.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}

	// Set RLS context for this transaction
	accountID := AccountIDFromContext(ctx)
	if accountID != "" {
		if err := SetTenantContext(ctx, tx); err != nil {
			tx.Rollback()
			return fmt.Errorf("set tenant context: %w", err)
		}
	}

	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}

	return tx.Commit()
}

// QueryPlatform runs a deliberately CROSS-TENANT read with the RLS platform-read
// escape hatch enabled (migration 065). It's for the warm-cache LoadAll loaders,
// which load every tenant's rows with no app.current_account_id set — under a
// NOBYPASSRLS app role (security #77) those would otherwise be filtered to
// nothing. `SET LOCAL` inside a read-only tx so the flag auto-resets when the tx
// ends (no leak onto the pooled connection).
//
// The tx is held OPEN until closeFn runs: lib/pq invalidates *sql.Rows once the
// tx commits/rolls back, so the caller MUST `defer closeFn()` and finish
// scanning before it fires. (The retired QueryRead committed before returning
// rows — under lib/pq that silently yields ZERO rows; it was dead code, removed.)
//
// A no-op under the current superuser (RLS is bypassed, so the hatch is never
// consulted), so wiring the loaders to it changes nothing today; it's what makes
// them keep working once the app role is downgraded.
func (s *Store) QueryPlatform(ctx context.Context, query string, args ...any) (*sql.Rows, func(), error) {
	tx, err := s.read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, nil, err
	}
	if _, err := tx.ExecContext(ctx, "SELECT set_config('app.platform_read', 'on', true)"); err != nil {
		tx.Rollback()
		return nil, nil, err
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		tx.Rollback()
		return nil, nil, err
	}
	return rows, func() { rows.Close(); tx.Rollback() }, nil
}

// QueryRowPlatform is the single-row form of QueryPlatform: scan runs inside the
// read-only platform-read tx (the *sql.Row can't outlive the tx under lib/pq).
// sql.ErrNoRows propagates out of row.Scan exactly as with a plain QueryRow.
func (s *Store) QueryRowPlatform(ctx context.Context, scan func(*sql.Row) error, query string, args ...any) error {
	tx, err := s.read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "SELECT set_config('app.platform_read', 'on', true)"); err != nil {
		return err
	}
	return scan(tx.QueryRowContext(ctx, query, args...))
}

// ExecPlatform runs a write (UPDATE/DELETE/INSERT) under the platform read
// hatch — for cross-tenant background jobs (e.g. the onboarding retention
// sweep stamping swept_at across every tenant's ingest jobs) whose target
// tables have USING-only tenant_isolation policies, so app.platform_read='on'
// admits the write too. `SET LOCAL` inside a tx so the flag auto-resets. Only
// use where the write is legitimately tenant-spanning; a per-account write must
// set app.current_account_id instead. No-op under the superuser.
func (s *Store) ExecPlatform(ctx context.Context, query string, args ...any) (sql.Result, error) {
	tx, err := s.primary.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "SELECT set_config('app.platform_read', 'on', true)"); err != nil {
		return nil, err
	}
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return res, tx.Commit()
}

// QueryTenantDB runs an account-scoped read on a raw *sql.DB with the tenant
// GUC set — the read-side twin of QueryPlatform, for the management List/Get
// handlers that filter by account_id but must also set app.current_account_id so
// RLS admits their rows under the NOBYPASSRLS app role (security #77). The tx is
// held OPEN until closeFn runs: defer it and finish scanning first (lib/pq
// invalidates *sql.Rows once the tx ends). No-op under the superuser.
func QueryTenantDB(ctx context.Context, db *sql.DB, accountID, query string, args ...any) (*sql.Rows, func(), error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, nil, err
	}
	if _, err := tx.ExecContext(ctx, "SELECT set_config('app.current_account_id', $1, true)", accountID); err != nil {
		tx.Rollback()
		return nil, nil, err
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		tx.Rollback()
		return nil, nil, err
	}
	return rows, func() { rows.Close(); tx.Rollback() }, nil
}

// ExecTenantDB runs an account-scoped write on a raw *sql.DB with the tenant
// GUC set — the write twin of QueryTenantDB. Without it every mutation on an
// RLS-policied table needs a hand-rolled tx + set_config dance, and the sites
// that skipped it matched zero rows silently under the NOBYPASSRLS app role.
func ExecTenantDB(ctx context.Context, db *sql.DB, accountID, query string, args ...any) (sql.Result, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "SELECT set_config('app.current_account_id', $1, true)", accountID); err != nil {
		return nil, err
	}
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return res, tx.Commit()
}

// QueryRowTenantDB is the single-row form of QueryTenantDB: scan runs inside a
// read-only tx with the caller's app.current_account_id set (security #77).
// sql.ErrNoRows propagates out of scan as usual. No-op under the superuser.
func QueryRowTenantDB(ctx context.Context, db *sql.DB, accountID string, scan func(*sql.Row) error, query string, args ...any) error {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "SELECT set_config('app.current_account_id', $1, true)", accountID); err != nil {
		return err
	}
	return scan(tx.QueryRowContext(ctx, query, args...))
}

// Ping checks both primary and read connections.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.primary.PingContext(ctx); err != nil {
		return fmt.Errorf("primary ping: %w", err)
	}
	if err := s.read.PingContext(ctx); err != nil {
		return fmt.Errorf("read ping: %w", err)
	}
	return nil
}
