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
	"os"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	_ "github.com/lib/pq"
)

func main() {
	profile := flag.String("profile", "standard", "seed profile name (label only, all YAMLs are loaded)")
	dir := flag.String("dsps-dir", "profiles/dsps", "directory containing DSP YAML profiles")
	pubDir := flag.String("publishers-dir", "profiles/publishers", "directory containing publisher YAML profiles")
	dealsDir := flag.String("deals-dir", "profiles/deals", "directory containing deal YAML profiles")
	flag.Parse()

	log := logger.New(constants.ServiceSeed)
	sc := config.Setup(constants.ServiceSeed, log)
	cfg := sc.Cfg

	dbURL := cfg.Get("database.url", "postgres://adtech:adtech-local-dev@localhost:5432/adtech?sslmode=disable")
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

	in := &inserter{db: db, log: log}
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
	log.Info("seed complete",
		"profile_label", *profile,
		"campaigns", campTotal,
		"publishers", pubTotal,
		"placements", plTotal,
		"deals", dealTotal,
	)
	fmt.Printf("seeded %d campaigns, %d publishers, %d placements, %d deals\n",
		campTotal, pubTotal, plTotal, dealTotal)
}
