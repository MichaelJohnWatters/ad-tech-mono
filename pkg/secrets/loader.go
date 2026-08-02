package secrets

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/lib/pq"
)

// PostgresLoader reads secrets a single service needs into its warm
// cache. Filtered by (purpose, owner) via FilterFor so each service only
// sees what its middleware actually validates.
//
// Self-healing: holds a dbURL rather than a pre-opened *sql.DB so it
// can re-open the connection on every LoadAll if Postgres was down at
// boot. The cache's poll loop calls LoadAll every 30s; if Postgres
// was unreachable at boot but comes back later, the next poll
// reconnects and rows start flowing — operator doesn't have to bounce
// the service. This pattern matters in dev where Colima can drop and
// take Postgres with it, and any service that booted with Postgres
// unreachable would otherwise be stuck on an empty cache forever.
//
// Lives here (in pkg/secrets) rather than pkg/store/postgres because the
// warm cache helper needs to construct the loader, and pkg/store/postgres
// can't import pkg/secrets without a cycle. Trade-off: this loader holds
// its own *sql.DB instead of sharing the read/write pool in pkg/store —
// fine given the secrets read load is tiny (one query per poll cycle).
type PostgresLoader struct {
	DBURL       string
	ServiceName string
	Log         *slog.Logger

	// Cipher decrypts the at-rest value column. Nil = passthrough
	// (plaintext), which is the supported local-dev mode. Set from
	// SECRETS_ENCRYPTION_KEY by pickLoader.
	Cipher *Cipher

	mu sync.Mutex
	db *sql.DB
}

// ensureDB opens (or re-opens) the underlying *sql.DB. Idempotent and
// thread-safe. Returns an error only when the connection can't be
// established right now; the caller decides whether to bubble that to
// the warm cache (we don't, so a transient outage doesn't blank the
// snapshot).
func (l *PostgresLoader) ensureDB(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.db != nil {
		// Cheap aliveness check. If it fails close + drop so the
		// branch below re-opens.
		pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := l.db.PingContext(pingCtx)
		cancel()
		if err == nil {
			return nil
		}
		if l.Log != nil {
			l.Log.Warn("secrets loader: existing DB ping failed, will reopen", "error", err)
		}
		_ = l.db.Close()
		l.db = nil
	}
	db, err := sql.Open("postgres", l.DBURL)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return fmt.Errorf("ping: %w", err)
	}
	db.SetMaxOpenConns(3)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(5 * time.Minute)
	l.db = db
	return nil
}

func (l *PostgresLoader) LoadAll(ctx context.Context) ([]Secret, error) {
	filter := FilterFor(l.ServiceName)
	if len(filter.Purposes) == 0 {
		return nil, nil
	}
	if err := l.ensureDB(ctx); err != nil {
		// Postgres unreachable — log and return empty so the warm
		// cache keeps serving whatever it already has cached. Next
		// poll tick retries. This is the self-heal path that fires
		// when Colima drops and comes back: any service that booted
		// before Postgres recovered would otherwise be stuck on the
		// empty initial load forever.
		if l.Log != nil {
			l.Log.Warn("secrets loader: postgres unreachable, will retry on next poll",
				"service", l.ServiceName, "error", err)
		}
		return nil, nil
	}
	l.mu.Lock()
	db := l.db
	l.mu.Unlock()
	if db == nil {
		// Race: ensureDB above set l.db, but a concurrent LoadAll (poll tick,
		// manual refresh, and NATS invalidate can all fire at once when
		// Postgres flaps) pinged, failed, and dropped the pool (l.db = nil)
		// before we read it. Treat like unreachable — serve the cached
		// snapshot, retry next tick — rather than nil-deref db.QueryContext
		// and crash the whole service.
		if l.Log != nil {
			l.Log.Warn("secrets loader: db pool dropped concurrently, will retry on next poll",
				"service", l.ServiceName)
		}
		return nil, nil
	}

	const q = `
SELECT
    id::text,
    name,
    value,
    purpose,
    owner,
    account_id::text,
    status,
    rotated_at,
    revokes_at,
    expires_at
FROM secrets
WHERE purpose = ANY($1::text[])
  AND owner IN ('platform', $2)
  AND status != 'revoked'`

	rows, err := db.QueryContext(ctx, q,
		pq.StringArray(filter.Purposes),
		l.ServiceName,
	)
	if err != nil {
		return nil, fmt.Errorf("query secrets: %w", err)
	}
	defer rows.Close()

	var out []Secret
	for rows.Next() {
		var s Secret
		var rotatedAt, revokesAt, expiresAt sql.NullTime
		var accountID sql.NullString
		if err := rows.Scan(
			&s.ID, &s.Name, &s.Value, &s.Purpose, &s.Owner, &accountID, &s.Status,
			&rotatedAt, &revokesAt, &expiresAt,
		); err != nil {
			return nil, fmt.Errorf("scan secret: %w", err)
		}
		if accountID.Valid {
			s.AccountID = accountID.String
		}
		if rotatedAt.Valid {
			t := rotatedAt.Time
			s.RotatedAt = &t
		}
		if revokesAt.Valid {
			t := revokesAt.Time
			s.RevokesAt = &t
		}
		if expiresAt.Valid {
			t := expiresAt.Time
			s.ExpiresAt = &t
		}
		// Decrypt the at-rest value. A key mismatch (wrong/rotated
		// SECRETS_ENCRYPTION_KEY) means we'd otherwise cache garbage and
		// silently reject every credential — log loudly and drop the row
		// so the failure is visible rather than mysterious 401s.
		plain, derr := l.Cipher.Decrypt(s.Value)
		if derr != nil {
			if l.Log != nil {
				l.Log.Error("secrets loader: decrypt failed, dropping row",
					"id", s.ID, "name", s.Name, "purpose", s.Purpose, "error", derr)
			}
			continue
		}
		s.Value = plain
		out = append(out, s)
	}
	return out, rows.Err()
}

// KeyOf returns the secret's row id.
func (l *PostgresLoader) KeyOf(s Secret) string { return s.ID }
