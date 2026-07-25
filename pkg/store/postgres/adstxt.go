package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/fraud"
)

// AdsTxtLoader reads the ads_txt_cache table into the exchange's warm cache
// so seller-authorisation checks run off in-memory data, not a per-auction
// DB hit. The table is keyed by publisher domain (global, no tenant scope).
type AdsTxtLoader struct {
	Store *Store
}

func (l *AdsTxtLoader) LoadAll(ctx context.Context) ([]fraud.AdsTxtRecord, error) {
	const q = `SELECT domain, entries, status FROM ads_txt_cache`
	rows, closeRows, err := l.Store.QueryPlatform(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query ads_txt_cache: %w", err)
	}
	defer closeRows()

	var out []fraud.AdsTxtRecord
	for rows.Next() {
		var domain, status string
		var entriesJSON []byte
		if err := rows.Scan(&domain, &entriesJSON, &status); err != nil {
			return nil, fmt.Errorf("scan ads_txt row: %w", err)
		}
		var entries []fraud.AdsTxtEntry
		if len(entriesJSON) > 0 {
			if err := json.Unmarshal(entriesJSON, &entries); err != nil {
				return nil, fmt.Errorf("unmarshal ads_txt entries for %s: %w", domain, err)
			}
		}
		out = append(out, fraud.AdsTxtRecord{Domain: domain, Entries: entries, Status: status})
	}
	return out, rows.Err()
}

func (l *AdsTxtLoader) KeyOf(r fraud.AdsTxtRecord) string { return r.Domain }
