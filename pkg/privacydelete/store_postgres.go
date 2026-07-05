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
type PostgresStore struct{ db *sql.DB }

// NewPostgresStore wraps an open *sql.DB.
func NewPostgresStore(db *sql.DB) *PostgresStore { return &PostgresStore{db: db} }

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
	return Purge{IdentityEdges: edges, SegmentMembers: members}, nil
}

func (s *PostgresStore) MarkCompleted(ctx context.Context, userID string, p Purge) error {
	if s.db == nil {
		return sql.ErrConnDone
	}
	systems, _ := json.Marshal(map[string]int{
		SystemIdentityGraph:  p.IdentityEdges,
		SystemSegmentMembers: p.SegmentMembers,
	})
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
		err := s.db.QueryRowContext(ctx, c.query, userID).Scan(&one)
		switch {
		case err == sql.ErrNoRows:
			// clean
		case err != nil:
			return nil, fmt.Errorf("residual check %s: %w", c.system, err)
		default:
			residual = append(residual, c.system)
		}
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
