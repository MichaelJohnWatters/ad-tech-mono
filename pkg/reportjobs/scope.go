package reportjobs

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
)

// ScopeLookup resolves what a tenant may query: its account type and (for
// publisher accounts) the publisher IDs it owns.
type ScopeLookup interface {
	AccountType(ctx context.Context, accountID string) (string, error)
	PublisherIDs(ctx context.Context, accountID string) ([]string, error)
}

// ResolveTenantFilters returns filters rewritten so the query can only see
// the account's own slice — the worker-side mirror of the gateway's
// enforceReportTenant middleware (cmd/gateway/reports_scope.go), applied when
// a schedule enqueues a job with no session in sight:
//
//   - advertiser/agency: filters.account_id forced to the account.
//   - publisher: filters.publisher_id must be one the account owns (verified);
//     absent with exactly one publisher it is injected; absent with several is
//     an error (the analytics filter model is single-valued).
//   - staff/admin: untouched, platform-wide.
//
// The input map is not mutated.
func ResolveTenantFilters(ctx context.Context, lookup ScopeLookup, accountID string, filters map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(filters)+1)
	for k, v := range filters {
		out[k] = v
	}
	accType, err := lookup.AccountType(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("account type for %s: %w", accountID, err)
	}
	switch auth.AccountType(accType) {
	case auth.AccountAdvertiser, auth.AccountAgency:
		out["account_id"] = accountID

	case auth.AccountPublisher:
		owned, err := lookup.PublisherIDs(ctx, accountID)
		if err != nil {
			return nil, fmt.Errorf("publisher lookup for %s: %w", accountID, err)
		}
		ownedSet := map[string]bool{}
		for _, id := range owned {
			ownedSet[id] = true
		}
		if want := out["publisher_id"]; want != "" {
			if !ownedSet[want] {
				return nil, fmt.Errorf("publisher %s not owned by account %s", want, accountID)
			}
		} else if len(owned) == 1 {
			out["publisher_id"] = owned[0]
		} else {
			return nil, fmt.Errorf("publisher_id filter required (account %s has %d publishers)", accountID, len(owned))
		}

	case auth.AccountStaff, auth.AccountAdmin:
		// Platform users query platform-wide.

	default:
		return nil, fmt.Errorf("unknown account type %q for %s", accType, accountID)
	}
	return out, nil
}

// PostgresScopeLookup resolves scope from the accounts/publishers tables.
type PostgresScopeLookup struct{ DB *sql.DB }

// AccountType returns the account's type (advertiser/publisher/agency/...).
func (s PostgresScopeLookup) AccountType(ctx context.Context, accountID string) (string, error) {
	if s.DB == nil {
		return "", sql.ErrConnDone
	}
	var t string
	err := s.DB.QueryRowContext(ctx,
		`SELECT type FROM accounts WHERE id = $1::uuid`, accountID).Scan(&t)
	return t, err
}

// PublisherIDs lists the non-archived publishers the account owns.
//
// RLS: callers are often NOT the tenant (staff impersonation, report-runner
// scoping), and the app role is NOBYPASSRLS — a bare query silently blanks to
// zero rows (the "account has multiple publishers" red herring, 2026-08-05).
// Scope the read to the target account; authorization happened upstream.
func (s PostgresScopeLookup) PublisherIDs(ctx context.Context, accountID string) ([]string, error) {
	if s.DB == nil {
		return nil, sql.ErrConnDone
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // read-only tx
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, accountID); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT id::text FROM publishers WHERE account_id = $1::uuid AND status != 'archived'`, accountID)
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
