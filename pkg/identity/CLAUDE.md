# pkg/identity - Identity Graph Vocabulary + Household Derivation

Shared vocabulary for the identity graph (edge sources, link types, confidences) plus the household-id derivation every service uses. The *persisted* graph lives in Postgres (`identity_graph` table, `pkg/store/postgres/identity.go`), built by `cmd/identity-consumer` — this package holds the constants and pure functions everyone agrees on.

## Key Entry Points

- **Signal source constants** (`identity.go`) — `SourceUID2`, `SourceHashedEmail`, `SourcePublisherUserID`, `SourceAdvertiserUserID`, `SourceDeviceID`, `SourceProbabilistic`, `SourceHousehold`. The `identity_graph.source` column is free text; these constants keep writers (identity-consumer via `pkg/identityobserve`) and readers (DSP resolver, profile-builder) aligned.
- **Link type constants** (`identity.go`) — `LinkCrossPublisher`, `LinkCrossDevice`, `LinkCRMMatch`, `LinkObserved` (two ids co-occurring on one request), `LinkHousehold`.
- **`HouseholdID(salt, ip)`** (`household.go`) — the platform household id: HMAC-SHA256(salt, ip), truncated to 16 hex chars, prefixed `HouseholdIDPrefix` (`"hh:"`). Returns `""` for empty IP = no household signal.
- **`HouseholdConfidence = 0.90`** — the edge confidence for household membership (IP-derived; below hashed_email because of VPNs/NAT).
- **`Graph`** (`identity.go`) — an in-memory graph (`Link`/`Resolve`/`Profile`/`Delete`/`Stats`). Reference implementation only: `pkg/privacy.Manager` accepts one, but no service constructs it on a serving path; production reads go through the Postgres store's adjacency loader (`LoadIdentityGraph`) into per-service snapshots.
- **`GeneratePlatformID()`** — new `pid-…` platform id.

## Invariants & Gotchas

- **One household deriver.** Every caller (SSP `cmd/ssp/main.go`, tracker `cmd/tracker/main.go`, seed, tests) MUST call `HouseholdID` with the same salt — `ssp.household_salt` (`keys.SSP.HouseholdSalt`, TierSecret) — or household ids won't line up across services. Rotating the salt changes every household id.
- **The `hh:` prefix is load-bearing.** Household nodes ride the same columns as user ids (graph, audience members, EIDs); the prefix is how readers tell them apart — `pkg/profilebuilder/unionfind.go` excludes `hh:`-prefixed nodes/edges from person clustering.
- **Confidence model, floored by readers.** Deterministic edges = 1.0, household = 0.90, probabilistic (IP+UA) = `identity_consumer.probabilistic_confidence` (default 0.5). Readers gate traversal: `dsp.identity_min_confidence` (bid-time resolver) and `profile_builder.min_confidence` (clustering). Raise to 0.8 to traverse deterministic-only.
- **Writes flow through NATS, not this package.** Services observing identity signals publish `adtech.identity.observed` (`events.SubjectIdentityObserved`) via `pkg/identityobserve.Publisher`; only `cmd/identity-consumer` writes edges. The SSP publish is consent-gated (`Personalise`) and fired in a goroutine off the serve path — the JetStream ack can block up to 1s and MUST NOT delay the auction (`cmd/ssp/identity.go`).
- **Hot-path rule.** The DSP resolves identity from a periodically refreshed in-memory snapshot (`cmd/dsp/identity.go`); the bid path never touches Postgres. The resolver is lazily constructed when `dsp.identity_resolution_enabled` reads true (boot-latch doctrine — don't latch the boot value).
- **`identity_graph` is platform-global — no `account_id`, no RLS.** Links cross tenants by design (`pkg/store/postgres/identity.go`); don't "fix" it with a tenant filter, and the gateway link-ingest API deliberately stores no account.
- **Upsert overwrites, not max.** `LinkIdentity` is idempotent on (user_id, linked_id, source); a re-link REPLACES confidence + created_at (no GREATEST). Self-links and empty ids are skipped; confidence outside (0,1] is coerced to 1.0.

## Used By

- `cmd/ssp`, `cmd/tracker` — `HouseholdID` + source constants when publishing observations
- `cmd/identity-consumer` (via `pkg/identityobserve`) — the only edge writer; see its CLAUDE.md (1 replica: in-memory fingerprint buckets)
- `cmd/dsp` — snapshot resolver over the Postgres graph
- `cmd/gateway` — identity link ingest API (`identity_links.go`)
- `pkg/identityobserve`, `pkg/profilebuilder`, `pkg/privacy`

## Testing

Pure package — plain unit tests (`identity_test.go`, `household_test.go`), no fakes needed. Store-side graph logic has its own unit tests in `pkg/store/postgres/identity_test.go` (no build tag — not an integration test).

## Architecture Details

See `docs/PLAN.md` -> "Identity and First-Party Data" (incl. "Identity Layers", "Identity Graph").
