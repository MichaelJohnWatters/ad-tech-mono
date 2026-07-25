package config

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events/natsbus"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	_ "github.com/lib/pq"
)

// applyDBPoolKnobsLive seeds the pool sizes at boot and subscribes to
// changes so UI edits to database.max_open_conns / database.max_idle_conns
// hit *sql.DB.SetMax{Open,Idle}Conns within one poll cycle. Both keys are
// TierLive in defaultSchema for this reason.
func applyDBPoolKnobsLive(db *sql.DB, cfg *Config, mgr *Manager, log *slog.Logger) {
	apply := func() {
		open := cfg.GetInt("database.max_open_conns", 10)
		idle := cfg.GetInt("database.max_idle_conns", 5)
		db.SetMaxOpenConns(open)
		db.SetMaxIdleConns(idle)
	}
	apply()
	mgr.OnChange("database.max_open_conns", func(_, _, newVal string) {
		if n, err := strconv.Atoi(newVal); err == nil {
			db.SetMaxOpenConns(n)
			log.Info("db pool resized", "max_open_conns", n)
		}
	})
	mgr.OnChange("database.max_idle_conns", func(_, _, newVal string) {
		if n, err := strconv.Atoi(newVal); err == nil {
			db.SetMaxIdleConns(n)
			log.Info("db pool resized", "max_idle_conns", n)
		}
	})
}

// AppVersion is the current platform version. Updated on release.
const AppVersion = "v1.0"

// ServiceConfig holds the standard config setup for any service.
type ServiceConfig struct {
	Cfg      *Config
	Manager  *Manager
	Registry *Registry
}

// SetupOption customises Setup behaviour. Constructed via the With* helpers.
type SetupOption func(*setupOpts)

type setupOpts struct {
	seedDefaults map[string]string // schema-key → boot-seed value override
}

// WithSeedDefaults overrides the schema-default values used by Registry.Register
// when it writes the first per-pod config row. Use when a single binary's
// SchemaEntry default doesn't match the pod-specific intent — most common
// case is the DSP service shipping with `dsp.noise_pct` default 0 but the
// `competitor1` profile needing 30. Without this override, the schema's 0
// gets seeded into Postgres for every DSP pod, masking the YAML default
// the operator expects.
//
// Only affects the seed step; the in-process schema (used for Validate
// and the schema_entries the UI displays) keeps its original defaults.
// Operator changes via PUT /v1/config also win — once a row exists for
// (pod_id, key), Registry.Register skips it on subsequent boots.
func WithSeedDefaults(overrides map[string]string) SetupOption {
	return func(o *setupOpts) {
		o.seedDefaults = overrides
	}
}

// Setup creates a Config + Manager with live polling and registers this
// pod's schema. Each service passes its owned []SchemaEntry; Setup merges
// it with the platform-shared defaults, registers the pod in Postgres
// (writing the full schema into service_registry.schema_entries), and
// starts the live-config poll.
//
// Usage:
//
//	sc := config.Setup("dsp", dspSchema, log)
//	sc.Manager.OnChange("dsp.daily_budget_default", func(key, old, new_ string) {
//	    // react to config change
//	})
//	port := sc.Cfg.Get("dsp.port", routes.PortDSP)
//
// Options:
//
//	sc := config.Setup("dsp", dspSchema, log,
//	    config.WithSeedDefaults(map[string]string{
//	        "dsp.noise_pct":   "30",
//	        "dsp.no_bid_rate": "0.20",
//	    }))
func Setup(serviceName string, schema []SchemaEntry, log *slog.Logger, opts ...SetupOption) *ServiceConfig {
	o := &setupOpts{}
	for _, apply := range opts {
		apply(o)
	}

	cfg := Load()

	// Merge this service's schema into the in-process registry so
	// Validate / GetDefault calls inside the service work locally. The
	// in-process schema keeps its ORIGINAL defaults — overrides apply
	// only to the per-pod seed step below, not to runtime fallbacks.
	Register(serviceName, schema)

	// Full schema written to the pod's registry row = service-owned keys
	// + the platform-shared defaults that apply to every service. Applies
	// any WithSeedDefaults overrides at this point so the first-boot
	// per-pod row picks up profile-specific values (e.g. dsp-competitor1
	// gets noise_pct=30 instead of the schema-wide 0 default the internal
	// DSP uses). Subsequent boots see the existing row and don't re-seed.
	fullSchema := make([]SchemaEntry, 0, len(schema)+24)
	for _, e := range schema {
		if e.Service == "" {
			e.Service = serviceName
		}
		if v, ok := o.seedDefaults[e.Key]; ok {
			e.Default = v
		}
		fullSchema = append(fullSchema, e)
	}
	fullSchema = append(fullSchema, defaultSchema()...)

	mgr := NewManager(cfg, log)

	pollInterval := cfg.GetDuration(serviceName+".config_poll_interval", 30*time.Second)
	mgr.SetPollInterval(pollInterval)

	// Try Postgres first for persistent config, fallback to memory.
	dbURL := cfg.Get("database_url", "")
	if dbURL == "" {
		dbURL = os.Getenv("DATABASE_URL")
	}
	if dbURL == "" {
		dbURL = routes.DefaultPostgresURL
	}
	// Mirror the resolved URL into cfg so warm-cache loaders, audience
	// stores, etc. that read cfg.Get("database.url") see the same value.
	cfg.SetLive("database.url", dbURL)

	// Pod-mode env-var overrides for service-to-service URLs and infra
	// endpoints. When services run as k8s pods (DEV_MODE=container /
	// prod), the Deployment YAML sets these env vars to in-cluster DNS
	// names like http://dsp.adtech.svc.cluster.local:8082. When unset
	// (DEV_MODE=fast / host processes), services fall back to the
	// Default*URL literals via cfg.Get("...url", routes.DefaultXxxURL).
	//
	// Mapping each env var to its corresponding config key here means
	// no service-code changes are required for the pod migration —
	// every consumer that already reads `cfg.Get("dsp.url", ...)` etc.
	// automatically picks up the env-injected value.
	//
	// See docs/PODS_MIGRATION.md for the full migration plan.
	envKeyMap := []struct{ env, key string }{
		{"DSP_URL", "dsp.url"},
		{"EXCHANGE_URL", "exchange.url"},
		{"SSP_URL", "ssp.url"},
		{"TRACKER_URL", "tracker.url"},
		{"REPORTING_URL", "reporting.url"},
		{"ADSERVER_URL", "adserver.url"},
		{"PUBLISHER_ADSERVER_URL", "publisher_adserver.url"},
		{"NATS_URL", "nats.url"},
		{"REDIS_URL", "redis.url"},
		{"EXCHANGE_DSP_ENDPOINTS", "exchange.dsp_endpoints"},

		// Service-prefixed infra aliases: cmd/exchange reads
		// "exchange.nats_url" not "nats.url", same for every service.
		// We bridge NATS_URL/REDIS_URL to each service's prefixed key so
		// the deployment YAMLs don't have to enumerate every alias.
		{"NATS_URL", serviceName + ".nats_url"},
		{"REDIS_URL", serviceName + ".redis_addr"},
	}
	// Service-prefixed env vars take precedence so a Deployment that
	// only sets TRACKER_NATS_URL (and not NATS_URL) still populates
	// nats.url and tracker.nats_url. Apply the prefix-fallback first so
	// the literal-env-var pass below can override it if both are set.
	// Bridged values land in the ENV layer (os.Setenv on the key's own env
	// form), NOT the live layer. They used to go in via SetLive, which made
	// them indistinguishable from Postgres rows — the manager's snapshot
	// poll (which is authoritative for the live layer, so deleted rows
	// actually revert) wiped them on the first tick and services fell back
	// to localhost defaults in-cluster. As env-layer values they survive
	// every poll, and a real live row still overrides them — the documented
	// defaults → env → live precedence, now actually true for bridges.
	bridge := func(key, v string) {
		if err := os.Setenv(envKeyFromConfigKey(key), v); err != nil {
			log.Warn("env bridge failed", "key", key, "error", err)
		}
	}
	upperService := strings.ToUpper(strings.ReplaceAll(serviceName, "-", "_"))
	for _, infra := range []struct{ envSuffix, key string }{
		{"NATS_URL", "nats.url"},
		{"NATS_URL", serviceName + ".nats_url"},
		{"REDIS_URL", "redis.url"},
		{"REDIS_URL", serviceName + ".redis_addr"},
	} {
		if v := os.Getenv(upperService + "_" + infra.envSuffix); v != "" {
			bridge(infra.key, v)
		}
	}
	for _, m := range envKeyMap {
		if v := os.Getenv(m.env); v != "" {
			bridge(m.key, v)
		}
	}

	// Initial Postgres attach. If postgres is unreachable at boot
	// (cluster start-up race, brief outage during a recovery, etc.)
	// we fall back to MemorySource so the service still boots — but
	// also spawn a background goroutine that retries the attach
	// indefinitely. Once postgres is reachable, the goroutine swaps
	// the manager's source to PostgresSource and runs the same
	// registry-register + seed-migration steps the boot path would
	// have. Without this, pods that boot during a postgres flap stay
	// on MemorySource forever and silently lose every config write.
	var registry *Registry
	db, err := sql.Open("postgres", dbURL)
	if err == nil {
		if pingErr := db.Ping(); pingErr == nil {
			registry = attachPostgres(mgr, cfg, db, serviceName, schema, fullSchema, o, envKeyMap, log, dbURL)
		} else {
			log.Warn("postgres unreachable, using memory config; retrying in background", "error", pingErr)
			mgr.SetSource(NewMemorySource(nil))
			db = nil
			go retryAttachPostgres(mgr, cfg, dbURL, serviceName, schema, fullSchema, o, envKeyMap, log)
		}
	} else {
		log.Warn("postgres open failed, using memory config; retrying in background", "error", err)
		mgr.SetSource(NewMemorySource(nil))
		db = nil
		go retryAttachPostgres(mgr, cfg, dbURL, serviceName, schema, fullSchema, o, envKeyMap, log)
	}

	// Wire NATS bus so PUT /v1/config from any pod broadcasts an invalidate
	// that this pod re-polls on. Without it the change still lands via the
	// 30s poll. Best-effort: NATS unavailable just means slower propagation.
	//
	// We don't EnsureStream here — the manager retries Subscribe on each
	// poll tick until it succeeds, so the order of "stream creation by
	// some service's main" vs "manager Subscribe" doesn't matter.
	natsURL := cfg.Get("nats.url", routes.DefaultNATSURL)
	if bus, err := natsbus.New(natsURL, serviceName+"-config", log); err == nil {
		mgr.SetBus(bus)
	} else {
		log.Warn("config bus unavailable, config changes will only land via poll", "error", err)
	}

	mgr.Start(context.Background())

	log.Info("config manager started", "service", serviceName, "poll_interval", pollInterval)

	return &ServiceConfig{Cfg: cfg, Manager: mgr, Registry: registry}
}

// attachPostgres performs the full "we have a working postgres handle"
// init sequence: SetSource(PostgresSource), live-tune the pool, build
// the registry, register the pod, run the seed-default + env-override
// migrations. Returns the registry (nil if registration failed). Used
// from the boot path and from retryAttachPostgres after a recovery.
func attachPostgres(mgr *Manager, cfg *Config, db *sql.DB, serviceName string, schema, fullSchema []SchemaEntry, o *setupOpts, envKeyMap []struct{ env, key string }, log *slog.Logger, dbURL string) *Registry {
	mgr.SetSource(NewPostgresSource(db))
	log.Info("config source: postgres", "url", dbURL)
	applyDBPoolKnobsLive(db, cfg, mgr, log)

	registry := NewRegistry(db, log)
	port := cfg.Get(serviceName+".port", "")
	if err := registry.Register(context.Background(), serviceName, AppVersion, port, fullSchema); err != nil {
		log.Warn("pod register failed", "error", err)
	}
	mgr.SetRegistry(registry, serviceName)

	migrateSeedDefaults(context.Background(), db, registry.PodID(), schema, o.seedDefaults, log)
	applyEnvOverridesToDB(context.Background(), db, registry.PodID(), schema, envKeyMap, log)
	return registry
}

// retryAttachPostgres loops in the background reopening + pinging the
// Postgres handle until it succeeds. On success, runs attachPostgres
// to swap the manager from MemorySource to PostgresSource. Without
// this, a pod that boots while Postgres is briefly down (cluster
// recovery, fresh-cluster race) stays on MemorySource forever and
// silently drops every config write — PUT looks successful to the
// gateway but the row never lands in Postgres.
//
// Backoff: 5 s, capped. We never give up — the alternative is to
// require a pod restart after every postgres flap, which defeats the
// "everything self-heals" platform promise.
func retryAttachPostgres(mgr *Manager, cfg *Config, dbURL, serviceName string, schema, fullSchema []SchemaEntry, o *setupOpts, envKeyMap []struct{ env, key string }, log *slog.Logger) {
	const retryInterval = 5 * time.Second
	for {
		time.Sleep(retryInterval)
		db, err := sql.Open("postgres", dbURL)
		if err != nil {
			continue
		}
		if err := db.Ping(); err != nil {
			_ = db.Close()
			continue
		}
		log.Info("postgres reachable, swapping config source from memory to postgres")
		attachPostgres(mgr, cfg, db, serviceName, schema, fullSchema, o, envKeyMap, log, dbURL)
		return
	}
}

// migrateSeedDefaults updates any per-pod config rows whose stored value
// matches the original schema default but whose seed override differs.
// Idempotent — once the row has the override value, the WHERE clause
// makes the UPDATE a no-op on subsequent boots. Operator-modified values
// (anything that doesn't match the original schema default) are preserved.
//
// Action="config_seed_migration" so the audit log distinguishes this
// from normal config_seed (first boot) and config_update (operator API).
func migrateSeedDefaults(ctx context.Context, db *sql.DB, podID string, schema []SchemaEntry, overrides map[string]string, log *slog.Logger) {
	for _, e := range schema {
		newVal, ok := overrides[e.Key]
		if !ok || newVal == e.Default {
			continue
		}
		// Postgres stores values as JSON-encoded strings — see PostgresSource.UpdateForPod.
		oldJSON, _ := json.Marshal(e.Default)
		newJSON, _ := json.Marshal(newVal)
		res, err := db.ExecContext(ctx, `
			UPDATE config
			SET value = $1, updated_at = now()
			WHERE pod_id = $2 AND key = $3 AND value = $4
		`, newJSON, podID, e.Key, oldJSON)
		if err != nil {
			log.Warn("seed-default migration failed", "key", e.Key, "pod", podID, "error", err)
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 {
			log.Info("seed-default migrated", "key", e.Key, "pod", podID, "old", e.Default, "new", newVal)
		}
	}
}

// applyEnvOverridesToDB overwrites Postgres rows whose value still equals
// the schema default with the corresponding env-var value. Mirrors the
// env→config bridge above so the value persists across the config poll —
// without this, registry.Register seeds e.g. exchange.dsp_endpoints with
// the schema default (localhost...) and Postgres beats the in-memory
// SetLive override on the next poll, leaving pod-mode services dialing
// their own loopback. Skipped on operator-modified rows: the WHERE clause
// matches only the default-shaped value.
func applyEnvOverridesToDB(ctx context.Context, db *sql.DB, podID string, schema []SchemaEntry, mapping []struct{ env, key string }, log *slog.Logger) {
	defaults := make(map[string]string, len(schema))
	for _, e := range schema {
		defaults[e.Key] = e.Default
	}
	for _, m := range mapping {
		v := os.Getenv(m.env)
		if v == "" {
			continue
		}
		def, hasDefault := defaults[m.key]
		if !hasDefault {
			continue
		}
		if v == def {
			continue
		}
		oldJSON, _ := json.Marshal(def)
		newJSON, _ := json.Marshal(v)
		res, err := db.ExecContext(ctx, `
			UPDATE config
			SET value = $1, updated_at = now()
			WHERE pod_id = $2 AND key = $3 AND value = $4
		`, newJSON, podID, m.key, oldJSON)
		if err != nil {
			log.Warn("env-override write failed", "key", m.key, "pod", podID, "error", err)
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 {
			log.Info("env-override applied to db", "key", m.key, "pod", podID, "env", m.env, "value", v)
		}
	}
}
