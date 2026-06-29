package postgres

import (
	"context"
	"fmt"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/privacy"
)

// OptOutLoader reads the user opt-out registry into a serving-side warm
// cache (DSP / ad server) so consent can be enforced on the bid hot path
// without a per-request DB hit.
//
// opt_out_registry is a global table keyed by user_id (no account scope —
// a user's opt-out applies platform-wide), so there's no tenant filter
// here, unlike the campaign/deal loaders.
type OptOutLoader struct {
	Store *Store
}

func (l *OptOutLoader) LoadAll(ctx context.Context) ([]privacy.OptOut, error) {
	const q = `SELECT user_id, level FROM opt_out_registry`
	rows, err := l.Store.read.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query opt_out_registry: %w", err)
	}
	defer rows.Close()

	var out []privacy.OptOut
	for rows.Next() {
		var userID string
		var level int
		if err := rows.Scan(&userID, &level); err != nil {
			return nil, fmt.Errorf("scan opt-out: %w", err)
		}
		out = append(out, privacy.OptOut{UserID: userID, Level: privacy.LevelFromInt(level)})
	}
	return out, rows.Err()
}

func (l *OptOutLoader) KeyOf(o privacy.OptOut) string { return o.UserID }
