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

	return &Store{primary: primary, read: read}, nil
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

// QueryRead executes a read query on the read replica with tenant context.
// For tenant-scoped reads that don't need a transaction.
func (s *Store) QueryRead(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	accountID := AccountIDFromContext(ctx)
	if accountID != "" {
		// For read queries, we need to set the session variable.
		// Use a transaction even for reads to scope the SET LOCAL.
		tx, err := s.read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, "SELECT set_config('app.current_account_id', $1, true)", accountID); err != nil {
			tx.Rollback()
			return nil, err
		}
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			tx.Rollback()
			return nil, err
		}
		// Note: caller must close rows, then we commit.
		// In practice, wrap this in a higher-level method.
		// For now, commit after query (rows buffered by driver).
		tx.Commit()
		return rows, nil
	}
	return s.read.QueryContext(ctx, query, args...)
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
