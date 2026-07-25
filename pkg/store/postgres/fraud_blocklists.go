package postgres

import (
	"context"
	"fmt"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/fraud"
)

// BlocklistLoader reads the fraud_blocklists table into a serving-side warm
// cache (the tracker) so IP/UA blocks are enforced on the hot path without
// a per-request DB hit. fraud_blocklists is a global table (no account
// scope — a block applies platform-wide), so there's no tenant filter.
type BlocklistLoader struct {
	Store *Store
}

func (l *BlocklistLoader) LoadAll(ctx context.Context) ([]fraud.BlocklistEntry, error) {
	const q = `SELECT type, value FROM fraud_blocklists`
	rows, closeRows, err := l.Store.QueryPlatform(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query fraud_blocklists: %w", err)
	}
	defer closeRows()

	var out []fraud.BlocklistEntry
	for rows.Next() {
		var e fraud.BlocklistEntry
		if err := rows.Scan(&e.Type, &e.Value); err != nil {
			return nil, fmt.Errorf("scan blocklist: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (l *BlocklistLoader) KeyOf(e fraud.BlocklistEntry) string { return e.Type + ":" + e.Value }
