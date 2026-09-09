# pkg/config - Configuration Loading

Three-layer config for every service: code defaults → env vars → live Postgres rows (dashboard-editable), refreshed by a 30s snapshot poll + NATS invalidate. Every key is a typed handle declared once in `pkg/config/keys/`.

## Key Entry Points

- `config.Setup(serviceName, keys.<Svc>Schema(), log)` (`setup.go`) — the one call every `cmd/*` binary makes at boot. Returns `ServiceConfig{Cfg, Manager, Registry}`. Registers the pod + its full schema in `service_registry`, seeds live-tier rows, starts the poll. `WithSeedDefaults` overrides first-boot seed values for profile pods (e.g. competitor DSP `dsp.noise_pct`).
- Typed handles (`keys.go`, declared in `pkg/config/keys/`) — `keys.DSP.NoisePct.Get(cfg)`. `KeySet` builders couple name/type/default/tier/description; declaring the key IS registering it. `Raw*` builds handles for deliberately-unregistered keys (env-bridged URLs). Raw `cfg.Get*` only for runtime-computed key names.
- `Manager` (`manager.go`) — poll loop, `OnChange`/`OnAnyChange` callbacks, `Set/SetForPod/Remove/Rollback/ResetToDefault`, and `HTTPHandler()` (the `/v1/config` API behind the gateway: list, `?resolved=true` provenance, `?schema=true`, `?history=true`, PUT with `pod_id` scoping).
- `Live[T]` holders (`live.go`) — `NewLiveDuration(mgr, cfg, key, def)` etc.; atomic-pointer reads for TierLive values consumed inside constructed objects (a plain `cfg.Get*` at boot freezes the value). Bad edits keep the previous value (binder only swaps on clean parse).
- Tiers (`schema.go`): `TierLive` (Postgres-seeded, UI-editable), `TierStatic` (env/YAML only), `TierSecret` (env only, redacted in the UI).
- `PostgresSource` (`postgres.go`) + `Registry` (`registry.go`) — the `config` table (values are JSON-encoded strings) and pod registration/heartbeat/stale-prune.

## Invariants & Gotchas (CRITICAL)

- **Resolution order:** pod-scoped Postgres row → global row (`pod_id=''`) → env var → schema default. **Pod rows SHADOW global rows** — a forgotten per-pod override silently beats the global value you just set.
- **A live Postgres row beats helm env.** Bit us before (`exchange.dsp_endpoints`): the seeded row overrode the Deployment env var. `applyEnvOverridesToDB` rewrites rows still holding the schema default, but operator-modified rows win.
- **The poll is a SNAPSHOT** (`applySnapshot`): a deleted row reverts the key to env/schema default on the next tick. Corollary: never inject values via `SetLive` and expect them to persist — the poll wipes them. Setup's env bridges use `os.Setenv` (env layer) for exactly this reason.
- **Zero string-literal key reads.** Add keys to the owning service's KeySet in `pkg/config/keys/`; read through the handle. Duplicate keys or malformed defaults panic at package init. Defaults are declared in canonical string form (`"300s"`, not re-rendered) so seeded rows don't churn.
- **Ports are NOT config keys** (`platform.go`) — schema-seeding them made Postgres beat `DSP_PORT`-style env overrides. Ports live in `pkg/routes` + env.
- **Boot-retry doctrine:** Postgres down at boot → `MemorySource` + background `retryAttachPostgres` loop (never latch memory-mode forever — that silently dropped every config write). NATS invalidate subscribe retries each poll tick; uses ephemeral `SubscribeBroadcast` with a **per-replica** group (`podid.Replica()` hostname, NOT `Registry.PodID()`/POD_NAME which is shared across a service's replicas — grouping on it queue-groups them so only one re-polls per invalidate). Durable per-pod consumers here caused the 2026-07 JetStream orphan wedge.
- Writes broadcast `adtech.cache.invalidate.config` so sibling pods re-poll immediately; NATS-unavailable just degrades to the 30s poll.
- Schema validation failures return `ErrValidation` → gateway maps to 400, not 500.

## Used By

Every service and job (`cmd/*`) via `config.Setup`. The gateway proxies the config-manager UI/API; `pkg/config/keys/` has one file per service.

## Testing

`MemorySource` for unit tests (no Postgres mocking); `keys/keys_test.go` guards schema registration counts.

## Pointers

- `docs/PLAN.md` → "Configuration Management" (Config Layers, Live Config Store, How Services Load Config, Config Change Flow)
- Root `CLAUDE.md` → Services convention (defaults → env vars → live config)
