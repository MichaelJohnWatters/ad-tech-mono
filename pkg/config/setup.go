package config

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"time"

	_ "github.com/lib/pq"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// AppVersion is the current platform version. Updated on release.
const AppVersion = "v1.0"

// ServiceConfig holds the standard config setup for any service.
type ServiceConfig struct {
	Cfg      *Config
	Manager  *Manager
	Registry *Registry
}

// Setup creates a Config + Manager with live polling for a service.
// All services call this instead of raw config.Load().
//
// Usage:
//
//	sc := config.Setup("exchange", log)
//	sc.Manager.OnChange("exchange.bid_timeout", func(key, old, new_ string) {
//	    // react to config change
//	})
//	port := sc.Cfg.Get("exchange.port", routes.PortExchange)
func Setup(serviceName string, log *slog.Logger) *ServiceConfig {
	cfg := Load()

	mgr := NewManager(cfg, log)

	// Default poll interval, overridable via env
	pollInterval := cfg.GetDuration(serviceName+".config_poll_interval", 30*time.Second)
	mgr.SetPollInterval(pollInterval)

	// Try Postgres first for persistent config, fallback to memory
	dbURL := cfg.Get("database_url", "")
	if dbURL == "" {
		dbURL = os.Getenv("DATABASE_URL")
	}
	if dbURL == "" {
		dbURL = "postgres://adtech:adtech-local-dev@localhost:5432/adtech?sslmode=disable"
	}
	// Mirror the resolved URL into the live cfg so services that read
	// cfg.Get("database.url", "") at runtime (warm-cache loaders, audience
	// store, etc.) see the same value Setup used to establish the connection.
	// Without this, every service logs "database.url not set" and falls back
	// to empty in-memory caches even though Postgres is reachable.
	cfg.SetLive("database.url", dbURL)

	db, err := sql.Open("postgres", dbURL)
	if err == nil {
		if err := db.Ping(); err == nil {
			mgr.SetSource(NewPostgresSource(db))
			log.Info("config source: postgres", "url", dbURL)
		} else {
			log.Warn("postgres unreachable, using memory config", "error", err)
			mgr.SetSource(NewMemorySource(DefaultValues()))
		}
	} else {
		log.Warn("postgres unavailable, using memory config", "error", err)
		mgr.SetSource(NewMemorySource(DefaultValues()))
	}

	// Register pod and seed its config keys
	var registry *Registry
	if db != nil {
		registry = NewRegistry(db, log)
		port := cfg.Get(serviceName+".port", "")
		registry.Register(context.Background(), serviceName, AppVersion, port)
		mgr.SetRegistry(registry, serviceName)
	}

	// Start polling in background
	mgr.Start(context.Background())

	log.Info("config manager started", "service", serviceName, "poll_interval", pollInterval)

	return &ServiceConfig{Cfg: cfg, Manager: mgr, Registry: registry}
}

// DefaultValues returns the platform-wide config defaults.
//
// NOTE: Port keys are intentionally NOT seeded into Postgres. They are
// infrastructure-fixed (the K8s manifest decides the port), not runtime-
// tunable from the config manager UI. Seeding them would let a Postgres
// value override the DSP_PORT/SSP_PORT env vars that the Tiltfile relies
// on to give each pod (internal / competitor1 / competitor2) a distinct
// port. Defaults for ports live in the code (routes.PortDSP etc.) and
// the env var path in Config.Get is the override.
func DefaultValues() map[string]string {
	return map[string]string{
		// Gateway
		"gateway.jwt_signing_key":      "",
		"gateway.dsp_url":              routes.DefaultDSPURL,
		"gateway.ssp_url":              routes.DefaultSSPURL,
		"gateway.adserver_url":         routes.DefaultAdServerURL,
		"gateway.reporting_url":        routes.DefaultReportingURL,
		"gateway.exchange_url":         routes.DefaultExchangeURL,
		"gateway.tracker_url":          routes.DefaultTrackerURL,
		"gateway.config_poll_interval": "30s",

		// Exchange
		"exchange.channel":       constants.ChannelAll,
		"exchange.bid_timeout":   "100ms",
		"exchange.dsp_endpoints": routes.DefaultDSPURL + "," + routes.DefaultDSPComp1URL + "," + routes.DefaultDSPComp2URL,
		"exchange.nats_url":      routes.DefaultNATSURL,

		// DSP
		"dsp.profile": "internal",

		// Tracker
		"tracker.nats_url":      routes.DefaultNATSURL,
		"tracker.reporting_url": routes.DefaultReportingURL,
		"tracker.signing_key":   "adtech-dev-signing-key-change-in-prod",

		// SSP
		"ssp.exchange_url": routes.DefaultExchangeURL,

		// Ad Server
		"adserver.tracker_url": routes.DefaultTrackerURL,

		// Reporting
		"reporting.nats_url": routes.DefaultNATSURL,
	}
}

// seedConfigDefaults inserts default config values into Postgres if the table is empty.
func seedConfigDefaults(db *sql.DB, log *slog.Logger) {
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM config").Scan(&count)
	if err != nil {
		return // table might not exist yet
	}
	if count > 0 {
		return // already seeded
	}

	source := NewPostgresSource(db)
	ctx := context.Background()
	defaults := DefaultValues()
	seeded := 0
	for key, value := range defaults {
		if err := source.Update(ctx, key, value); err != nil {
			log.Warn("failed to seed config", "key", key, "error", err)
		} else {
			seeded++
		}
	}
	log.Info("seeded config defaults into postgres", "count", seeded)
}
