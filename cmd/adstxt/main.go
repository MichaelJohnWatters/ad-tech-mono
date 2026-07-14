// cmd/adstxt is the ads.txt crawler. Run as a daily CronJob: it lists every
// publisher domain, fetches https://{domain}/ads.txt, and upserts the parsed
// result into ads_txt_cache. The exchange warm-caches that table and enforces
// seller authorisation (exchange.adstxt_enforcement).
//
// One-shot: fetch all, write, exit. Kubernetes schedules the cadence.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events/natsbus"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/fraud"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	_ "github.com/lib/pq"
)

func main() {
	log := logger.New("adstxt")
	sc := config.Setup("adstxt", nil, log)
	cfg := sc.Cfg

	dbURL := keys.Database.URL.Get(cfg)
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Error("open postgres", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	domains, err := publisherDomains(ctx, db)
	if err != nil {
		log.Error("list publisher domains", "error", err)
		os.Exit(1)
	}
	log.Info("ads.txt crawl starting", "domains", len(domains))

	client := &http.Client{Timeout: 10 * time.Second}
	var valid, missing, errored, changed int
	for _, domain := range domains {
		rec := fraud.FetchAdsTxt(ctx, client, domain)
		didChange, err := upsert(ctx, db, rec)
		if err != nil {
			log.Error("upsert ads_txt_cache", "domain", domain, "error", err)
			continue
		}
		if didChange {
			changed++
		}
		switch rec.Status {
		case "valid":
			valid++
		case "missing":
			missing++
		default:
			errored++
		}
		log.Debug("crawled", "domain", domain, "status", rec.Status, "entries", len(rec.Entries), "changed", didChange)
	}
	log.Info("ads.txt crawl complete", "valid", valid, "missing", missing, "error", errored, "changed", changed)

	// If any domain's ads.txt actually moved, invalidate the exchange's warm
	// cache so it re-reads immediately instead of waiting for the 300s poll.
	// Best-effort: a NATS outage just means the poll picks it up later.
	if changed > 0 {
		bus, err := natsbus.New(keys.NATS.URL.Get(cfg), "adstxt", log)
		if err != nil {
			log.Warn("nats connect for ads.txt invalidate failed; exchange picks up on next poll", "error", err)
			return
		}
		defer bus.Close()
		if err := bus.Publish(ctx, events.SubjectCacheInvalidateAdsTxt, []byte(`{"source":"adstxt"}`)); err != nil {
			log.Warn("publish ads.txt invalidate failed", "error", err)
			return
		}
		log.Info("published ads.txt cache invalidate", "changed_domains", changed)
	}
}

func publisherDomains(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT domain FROM publishers WHERE domain <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// upsert writes the crawl result and reports whether the parsed ads.txt content
// actually changed (a brand-new domain counts as a change). Detection: on an
// insert or a content change, last_changed is set to now() — the same value as
// last_fetched in this statement — so (last_changed = last_fetched) is true iff
// the row changed this run.
func upsert(ctx context.Context, db *sql.DB, rec fraud.AdsTxtRecord) (bool, error) {
	entries := rec.Entries
	if entries == nil {
		entries = []fraud.AdsTxtEntry{}
	}
	b, err := json.Marshal(entries)
	if err != nil {
		return false, err
	}
	// last_changed advances only when the parsed content actually changed,
	// so ops can see "this publisher's ads.txt moved" vs "we just re-crawled".
	const q = `
INSERT INTO ads_txt_cache (domain, entries, last_fetched, status, last_changed)
VALUES ($1, $2, now(), $3, now())
ON CONFLICT (domain) DO UPDATE SET
    entries      = EXCLUDED.entries,
    last_fetched = now(),
    status       = EXCLUDED.status,
    last_changed = CASE WHEN ads_txt_cache.entries IS DISTINCT FROM EXCLUDED.entries
                        THEN now() ELSE ads_txt_cache.last_changed END
RETURNING (last_changed = last_fetched)`
	var changed bool
	if err := db.QueryRowContext(ctx, q, rec.Domain, b, rec.Status).Scan(&changed); err != nil {
		return false, err
	}
	return changed, nil
}
