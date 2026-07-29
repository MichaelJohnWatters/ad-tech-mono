package main

import (
	"context"
	"fmt"
)

// SeedAdsTxt authorises every seeded publisher's domain to sell through our
// exchange, so the platform works under strict ads.txt enforcement
// (exchange.adstxt_enforcement=strict, the prod-shaped default in values).
//
// In production the cmd/adstxt crawler is the source of truth — it fetches each
// publisher's real ads.txt file (which must list us) and populates ads_txt_cache.
// Locally there are no real ads.txt files, so the seed writes the authorising
// line directly, exactly as the crawler would after a successful fetch. The line
// mirrors the exchange's configured seller identity:
//
//	adtech.local, adtech-exchange, DIRECT
//
// The exchange's warm ads.txt cache polls Postgres every ~5m; callers that need
// it live immediately (make demo/reset, the e2e harness) hit the exchange's
// /debug/cache/refresh after seeding.
func (in *inserter) SeedAdsTxt(ctx context.Context, sellerDomain, sellerID string) (int, error) {
	// One authorising entry, keyed by the Go field names (fraud.AdsTxtEntry has
	// no json tags). Relationship DIRECT = the publisher sells this inventory
	// itself (vs RESELLER).
	entries := fmt.Sprintf(`[{"Domain":%q,"AccountID":%q,"Relationship":"DIRECT"}]`, sellerDomain, sellerID)

	// Authorise every publisher that has a non-empty domain. Idempotent.
	const q = `
INSERT INTO ads_txt_cache (domain, entries, status, last_fetched, last_changed)
SELECT DISTINCT domain, $1::jsonb, 'valid', now(), now()
  FROM publishers
 WHERE domain <> ''
ON CONFLICT (domain) DO UPDATE
   SET entries = EXCLUDED.entries, status = 'valid', last_fetched = now()`
	res, err := in.db.ExecContext(ctx, q, entries)
	if err != nil {
		return 0, fmt.Errorf("seed ads_txt_cache: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
