package postgres

import (
	"context"
	"database/sql"
	"fmt"
)

// DSPRow is the runtime view of a row in the dsps table. Used by the DSP
// service at boot to discover its own identity + behavior, and by the
// exchange to enumerate fan-out targets (future External DSP Partners work).
type DSPRow struct {
	ID          string
	Name        string
	DisplayName string
	ProfileType string  // 'internal' | 'competitor' | 'external'
	NoisePct    int
	NoBidRate   float64
	Endpoint    string  // empty for in-cluster pods
	Status      string  // 'active' | 'paused' | 'terminated'
}

// IsCompetitor is derived, not stored. A DSP "behaves like a competitor"
// whenever its noise or random-no-bid settings are non-zero — there's no
// separate boolean. (Previously dsp.is_competitor was a config key; that
// created a "true but with zero noise" inconsistency that this derivation
// makes impossible.)
func (d DSPRow) IsCompetitor() bool {
	return d.NoisePct > 0 || d.NoBidRate > 0
}

// DSPByName returns the dsps row whose name matches. Used by the DSP
// service at boot: each pod is configured with its "profile name" and
// looks itself up to get the ID + behavior knobs.
func DSPByName(ctx context.Context, db *sql.DB, name string) (*DSPRow, error) {
	const q = `
SELECT id::text, name, display_name, profile_type, noise_pct, no_bid_rate,
       COALESCE(endpoint, ''), status
FROM dsps WHERE name = $1`
	row := db.QueryRowContext(ctx, q, name)
	var d DSPRow
	err := row.Scan(&d.ID, &d.Name, &d.DisplayName, &d.ProfileType,
		&d.NoisePct, &d.NoBidRate, &d.Endpoint, &d.Status)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("dsp %q not found", name)
	}
	if err != nil {
		return nil, fmt.Errorf("lookup dsp %q: %w", name, err)
	}
	return &d, nil
}

// ListDSPs returns all DSPs, optionally filtered by status. Used by the
// exchange (future) to enumerate fan-out endpoints and by admin tooling.
func ListDSPs(ctx context.Context, db *sql.DB, statusFilter string) ([]DSPRow, error) {
	q := `SELECT id::text, name, display_name, profile_type, noise_pct,
              no_bid_rate, COALESCE(endpoint, ''), status FROM dsps`
	args := []any{}
	if statusFilter != "" {
		q += ` WHERE status = $1`
		args = append(args, statusFilter)
	}
	q += ` ORDER BY name`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list dsps: %w", err)
	}
	defer rows.Close()
	var out []DSPRow
	for rows.Next() {
		var d DSPRow
		if err := rows.Scan(&d.ID, &d.Name, &d.DisplayName, &d.ProfileType,
			&d.NoisePct, &d.NoBidRate, &d.Endpoint, &d.Status); err != nil {
			return nil, fmt.Errorf("scan dsp: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
