package privacydelete

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// PostgresStore implements Store against the platform tables. The user-data
// tables (identity_graph, audience_segment_members) are keyed by user_id and a
// deletion spans every tenant that ever saw the user, so the purge is a
// platform-wide (cross-tenant) write — like pkg/store/postgres.ContractLoader's
// cross-tenant read, it relies on the app role owning the tables (RLS is not
// enforced for the owner). opt_out_registry is a global, un-scoped table.
type PostgresStore struct {
	db *sql.DB
	// extras purge + verify user data held outside Postgres (Delta lake via
	// the pipeline, freq_cap_blocks in ClickHouse). Run after the Postgres
	// transaction commits; any failure fails PurgeUser so the registry row
	// stays pending and the next run retries.
	extras []ExtraPurger
}

// NewPostgresStore wraps an open *sql.DB.
func NewPostgresStore(db *sql.DB) *PostgresStore { return &PostgresStore{db: db} }

// WithExtras attaches non-Postgres purgers (chainable).
func (s *PostgresStore) WithExtras(extras ...ExtraPurger) *PostgresStore {
	s.extras = append(s.extras, extras...)
	return s
}

func (s *PostgresStore) PendingDeletions(ctx context.Context) ([]string, error) {
	return s.queryUserIDs(ctx,
		`SELECT user_id FROM opt_out_registry WHERE level = 3 AND completed_at IS NULL ORDER BY requested_at`)
}

func (s *PostgresStore) CompletedUnverified(ctx context.Context) ([]string, error) {
	return s.queryUserIDs(ctx,
		`SELECT user_id FROM opt_out_registry
		  WHERE level = 3 AND completed_at IS NOT NULL AND verified_at IS NULL
		  ORDER BY completed_at`)
}

func (s *PostgresStore) queryUserIDs(ctx context.Context, q string) ([]string, error) {
	if s.db == nil {
		return nil, sql.ErrConnDone
	}
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// PurgeUser deletes the user's identity-graph edges (either endpoint) and
// segment memberships in one transaction.
func (s *PostgresStore) PurgeUser(ctx context.Context, userID string) (Purge, error) {
	if s.db == nil {
		return Purge{}, sql.ErrConnDone
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Purge{}, err
	}
	defer tx.Rollback()

	// A GDPR purge spans every tenant that ever saw the user, so it is a
	// cross-tenant write. audience_segment_members carries the tenant_isolation
	// RLS policy (mig 019 + the mig 065 platform hatch); under the NOBYPASSRLS
	// app role the DELETE below would otherwise match ZERO rows and silently
	// certify a purge that deleted nothing. platform_read='on' (tx-local) opts
	// into the policy's escape hatch so the delete reaches every tenant's rows.
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.platform_read', 'on', true)`); err != nil {
		return Purge{}, fmt.Errorf("set platform_read: %w", err)
	}

	edges, err := execCount(ctx, tx,
		`DELETE FROM identity_graph WHERE user_id = $1 OR linked_id = $1`, userID)
	if err != nil {
		return Purge{}, fmt.Errorf("delete identity_graph: %w", err)
	}
	members, err := execCount(ctx, tx,
		`DELETE FROM audience_segment_members WHERE user_id = $1`, userID)
	if err != nil {
		return Purge{}, fmt.Errorf("delete audience_segment_members: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Purge{}, err
	}
	p := Purge{IdentityEdges: edges, SegmentMembers: members}
	for _, e := range s.extras {
		counts, err := e.PurgeExtra(ctx, userID)
		if err != nil {
			// The PG rows are already gone (idempotent deletes) — failing here
			// keeps the registry row pending so the whole purge re-runs.
			return Purge{}, fmt.Errorf("extra purge: %w", err)
		}
		if p.Extra == nil {
			p.Extra = map[string]int{}
		}
		for sys, n := range counts {
			p.Extra[sys] += n
		}
	}
	return p, nil
}

func (s *PostgresStore) MarkCompleted(ctx context.Context, userID string, p Purge) error {
	if s.db == nil {
		return sql.ErrConnDone
	}
	counts := map[string]int{
		SystemIdentityGraph:  p.IdentityEdges,
		SystemSegmentMembers: p.SegmentMembers,
	}
	for sys, n := range p.Extra {
		counts[sys] = n
	}
	systems, _ := json.Marshal(counts)
	_, err := s.db.ExecContext(ctx,
		`UPDATE opt_out_registry
		    SET completed_at = now(), systems_completed = $2::jsonb
		  WHERE user_id = $1`,
		userID, string(systems))
	return err
}

// Residual reports which user-data systems still hold rows for userID.
func (s *PostgresStore) Residual(ctx context.Context, userID string) ([]string, error) {
	if s.db == nil {
		return nil, sql.ErrConnDone
	}
	// Same cross-tenant scope as PurgeUser: the residual check reads
	// audience_segment_members (RLS) across every tenant, so it must set the
	// platform_read hatch or it would see zero rows on a pooled NOBYPASSRLS
	// connection and falsely certify a clean purge. Run both checks in one
	// tx-local hatch.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.platform_read', 'on', true)`); err != nil {
		return nil, fmt.Errorf("set platform_read: %w", err)
	}

	var residual []string
	checks := []struct {
		system string
		query  string
	}{
		{SystemIdentityGraph, `SELECT 1 FROM identity_graph WHERE user_id = $1 OR linked_id = $1 LIMIT 1`},
		{SystemSegmentMembers, `SELECT 1 FROM audience_segment_members WHERE user_id = $1 LIMIT 1`},
	}
	for _, c := range checks {
		var one int
		err := tx.QueryRowContext(ctx, c.query, userID).Scan(&one)
		switch {
		case err == sql.ErrNoRows:
			// clean
		case err != nil:
			return nil, fmt.Errorf("residual check %s: %w", c.system, err)
		default:
			residual = append(residual, c.system)
		}
	}
	for _, e := range s.extras {
		systems, err := e.ResidualExtra(ctx, userID)
		if err != nil {
			return nil, fmt.Errorf("extra residual check: %w", err)
		}
		residual = append(residual, systems...)
	}
	return residual, nil
}

func (s *PostgresStore) MarkVerified(ctx context.Context, userID string) error {
	if s.db == nil {
		return sql.ErrConnDone
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE opt_out_registry SET verified_at = now() WHERE user_id = $1`, userID)
	return err
}

func execCount(ctx context.Context, tx *sql.Tx, q, arg string) (int, error) {
	res, err := tx.ExecContext(ctx, q, arg)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
