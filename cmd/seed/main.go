// cmd/seed loads YAML profiles into Postgres so the runtime services
// can read everything from the database. Idempotent — re-run after editing
// a YAML and the same rows UPSERT in place (UUIDs are derived deterministically
// from the YAML external keys via DeriveID).
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects"
	objs3 "github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/s3"
	_ "github.com/lib/pq"
)

func main() {
	profile := flag.String("profile", "standard", "seed profile name (label only, all YAMLs are loaded)")
	dir := flag.String("dsps-dir", "profiles/dsps", "directory containing DSP YAML profiles")
	pubDir := flag.String("publishers-dir", "profiles/publishers", "directory containing publisher YAML profiles")
	dealsDir := flag.String("deals-dir", "profiles/deals", "directory containing deal YAML profiles")
	directSoldDir := flag.String("direct-sold-dir", "profiles/direct-sold", "directory containing publisher direct-sold line item profiles")
	flag.Parse()

	log := logger.New(constants.ServiceSeed)
	sc := config.Setup(constants.ServiceSeed, nil, log)
	cfg := sc.Cfg

	dbURL := cfg.Get("database.url", routes.DefaultPostgresURL)
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Error("open db", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		log.Error("ping db", "error", err)
		os.Exit(1)
	}

	profiles, err := LoadProfiles(*dir)
	if err != nil {
		log.Error("load profiles", "dir", *dir, "error", err)
		os.Exit(1)
	}
	if len(profiles) == 0 {
		log.Warn("no profiles found", "dir", *dir)
		return
	}

	pubProfiles, err := LoadPublisherProfiles(*pubDir)
	if err != nil {
		log.Error("load publisher profiles", "dir", *pubDir, "error", err)
		os.Exit(1)
	}
	dealProfiles, err := LoadDealProfiles(*dealsDir)
	if err != nil {
		log.Error("load deal profiles", "dir", *dealsDir, "error", err)
		os.Exit(1)
	}
	directSoldProfiles, err := LoadDirectSoldProfiles(*directSoldDir)
	if err != nil {
		log.Error("load direct-sold profiles", "dir", *directSoldDir, "error", err)
		os.Exit(1)
	}

	// Upload themed SVGs to the object store so the asset_url half
	// of the creative split resolves. Best-effort — if Minio is
	// unreachable, the inserter falls back to inline HTML for every
	// creative (degraded but functional). Asset base URL is the
	// browser-facing prefix (gateway proxy) so creatives.html_content
	// records reference a URL the browser can actually fetch.
	bucket := cfg.Get("s3.bucket", "adtech-creatives")
	objStore := connectObjectStore(ctx, cfg, log)
	if err := uploadCreativeAssets(ctx, objStore, bucket, log); err != nil {
		log.Warn("creative asset upload run failed", "error", err)
	}
	assetBase := cfg.Get("seed.creatives_url_base", "http://localhost:8080"+routes.ProxyCreatives[:len(routes.ProxyCreatives)-1])

	in := &inserter{db: db, log: log, creativeAssetBase: assetBase}
	if err := in.SeedAll(ctx, profiles); err != nil {
		log.Error("seed campaigns failed", "error", err)
		os.Exit(1)
	}
	if err := in.SeedPublishers(ctx, pubProfiles); err != nil {
		log.Error("seed publishers failed", "error", err)
		os.Exit(1)
	}
	if err := in.SeedDeals(ctx, dealProfiles); err != nil {
		log.Error("seed deals failed", "error", err)
		os.Exit(1)
	}
	if err := in.SeedDirectSold(ctx, directSoldProfiles); err != nil {
		log.Error("seed direct-sold failed", "error", err)
		os.Exit(1)
	}
	if err := in.SeedDevSecrets(ctx); err != nil {
		log.Error("seed dev secrets failed", "error", err)
		os.Exit(1)
	}

	campTotal, plTotal, dealTotal := 0, 0, 0
	for _, p := range profiles {
		campTotal += len(p.Campaigns)
	}
	pubTotal := 0
	for _, p := range pubProfiles {
		pubTotal += len(p.Publishers)
		for _, pub := range p.Publishers {
			plTotal += len(pub.Placements)
		}
	}
	for _, p := range dealProfiles {
		dealTotal += len(p.Deals)
	}
	directSoldTotal := 0
	for _, p := range directSoldProfiles {
		directSoldTotal += len(p.LineItems)
	}
	log.Info("seed complete",
		"profile_label", *profile,
		"campaigns", campTotal,
		"publishers", pubTotal,
		"placements", plTotal,
		"deals", dealTotal,
		"direct_sold", directSoldTotal,
	)
	fmt.Printf("seeded %d campaigns, %d publishers, %d placements, %d deals, %d direct-sold line items\n",
		campTotal, pubTotal, plTotal, dealTotal, directSoldTotal)
}

// connectObjectStore wires an S3/Minio client when s3.endpoint is
// configured, otherwise returns nil and lets the seed fall back to
// the inline-HTML path for every creative. Mirrors the adserver
// connectObjects helper but inlined here so cmd/seed stays small.
func connectObjectStore(ctx context.Context, cfg *config.Config, log *slog.Logger) objects.Store {
	endpoint := cfg.Get("s3.endpoint", "")
	if endpoint == "" {
		log.Warn("s3.endpoint not set, creative asset upload skipped (every creative will use inline HTML)")
		return nil
	}
	cli, err := objs3.New(objs3.Config{
		Endpoint:  trimScheme(endpoint),
		AccessKey: cfg.Get("s3.access_key", "minioadmin"),
		SecretKey: cfg.Get("s3.secret_key", "minioadmin"),
		UseSSL:    cfg.GetBool("s3.use_ssl", false),
	})
	if err != nil {
		log.Warn("s3 init failed; creative asset upload skipped", "error", err)
		return nil
	}
	return cli
}

// trimScheme strips http:// or https:// from a Minio endpoint URL
// because the minio-go client expects just host:port.
func trimScheme(s string) string {
	for _, prefix := range []string{"http://", "https://"} {
		if len(s) > len(prefix) && s[:len(prefix)] == prefix {
			return s[len(prefix):]
		}
	}
	return s
}
