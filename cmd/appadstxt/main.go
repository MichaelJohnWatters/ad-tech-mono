// cmd/appadstxt is the app-ads.txt crawler, the in-app sibling of cmd/adstxt.
// Run as a daily CronJob: it lists every app publisher's developer domain,
// fetches https://{developer_domain}/app-ads.txt, and upserts the parsed result
// into app_ads_txt_cache. Web-only publishers (empty developer_domain) are
// skipped. Enforcement / warm-cache consumption is a later step; this job just
// keeps the cache table current.
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
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/fraud"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	_ "github.com/lib/pq"
)

func main() {
	log := logger.New("appadstxt")
	sc := config.Setup("appadstxt", nil, log)
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

	domains, err := developerDomains(ctx, db)
	if err != nil {
		log.Error("list developer domains", "error", err)
		os.Exit(1)
	}
	log.Info("app-ads.txt crawl starting", "domains", len(domains))

	client := &http.Client{Timeout: 10 * time.Second}
	var valid, missing, errored, changed int
	for _, domain := range domains {
		rec := fraud.FetchAppAdsTxt(ctx, client, domain)
		didChange, err := upsert(ctx, db, rec)
		if err != nil {
			log.Error("upsert app_ads_txt_cache", "developer_domain", domain, "error", err)
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
		log.Debug("crawled", "developer_domain", domain, "status", rec.Status, "entries", len(rec.Entries), "changed", didChange)
	}
	log.Info("app-ads.txt crawl complete", "valid", valid, "missing", missing, "error", errored, "changed", changed)
}

func developerDomains(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT developer_domain FROM publishers WHERE developer_domain <> ''`)
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

// upsert writes the crawl result and reports whether the parsed app-ads.txt
// content actually changed (a brand-new domain counts as a change). Detection
// matches cmd/adstxt: last_changed advances to now() only on insert or content
// change, so (last_changed = last_fetched) is true iff the row changed this run.
func upsert(ctx context.Context, db *sql.DB, rec fraud.AppAdsTxtRecord) (bool, error) {
	entries := rec.Entries
	if entries == nil {
		entries = []fraud.AdsTxtEntry{}
	}
	b, err := json.Marshal(entries)
	if err != nil {
		return false, err
	}
	const q = `
INSERT INTO app_ads_txt_cache (developer_domain, entries, last_fetched, status, last_changed)
VALUES ($1, $2, now(), $3, now())
ON CONFLICT (developer_domain) DO UPDATE SET
    entries      = EXCLUDED.entries,
    last_fetched = now(),
    status       = EXCLUDED.status,
    last_changed = CASE WHEN app_ads_txt_cache.entries IS DISTINCT FROM EXCLUDED.entries
                        THEN now() ELSE app_ads_txt_cache.last_changed END
RETURNING (last_changed = last_fetched)`
	var changed bool
	if err := db.QueryRowContext(ctx, q, rec.DeveloperDomain, b, rec.Status).Scan(&changed); err != nil {
		return false, err
	}
	return changed, nil
}
