# pkg/events - Event Bus

The async messaging abstraction. Services NEVER import NATS directly — they import this package (interface + subjects + payloads) and the JetStream implementation in `natsbus/`. JSON payloads on the wire; every payload struct carries `schema_version` as its FIRST field (`CurrentSchemaVersion` in `payloads.go`, enforced by `payloads_test.go`).

## Key Entry Points

- `EventBus` (`events.go`) — Publish/Subscribe/Close. Implementations: `natsbus/` (JetStream), `memory.go` (in-process `MemoryBus` for tests/host runs; implements a size-1 `BatchSubscriber`).
- `Publisher` (`publisher.go`) — what services actually hold. Typed helpers (`AuctionWin`, `DSPCall`, …) stamp `SchemaVersion` and route through `publishJSONID`; `PublishJSONID` is the public entry for callers building their own envelopes (tracker beacons).
- `PublishDedup` / `PublisherWithID` (`events.go`) — publish with a stable `Nats-Msg-Id` so JetStream drops republishes server-side. Nil-bus guard lives here (nil bus at boot SIGSEGVed the tracker fleet 2026-08-05).
- `SubscribeBroadcast` / `BroadcastSubscriber` (`events.go`) — per-pod fan-out via an EPHEMERAL consumer (no Durable, 5m `InactiveThreshold`, self-recreates on fetch failure). THE way to consume cache invalidates.
- `BatchSubscriber` / `SubscribeBatch` — one JetStream fetch (≤500 msgs) delivered as a slice for bulk INSERTs; the handler owns per-message ack/nak.
- `IdempotentHandler` (`idempotent.go`) — Redis SetNX dedup wrapper; unmarks the key on handler failure so retries work.
- `Spool` + `Publisher.EnableSpool` (`spool.go`) — disk spool for failed publishes (`EVENT_SPOOL_DIR` emptyDir, 256MiB `DefaultSpoolCap`); background drainer replays with the spooled msg ID. `Pressure()` (0–100) feeds the SSP front-door shed via the exchange's `X-Event-Pressure` response header.
- `subjects.go` — ALL subject constants + stream identity (`StreamName` = `adtech`, `StreamSubjects` = `adtech.>`). `payloads.go` — all event structs.

## Invariants & Gotchas

- **New subject = constant in `subjects.go` + row in `docs/PLAN.md` → "NATS Subjects (Async Events)".** Never a string literal in a service.
- **Consumer names embed the FULL sanitised subject path** (`subjectToConsumerSuffix` in `natsbus/natsbus.go`: `adtech.auction.win` → `auction-win`). Leaf-only names collided: every `*.win` subject mapped to consumer "win", `CreateOrUpdateConsumer` overwrote the `FilterSubject`, and reporting silently dropped DirectWin/PrebidOutboundWin.
- **Cache invalidates MUST use `events.SubscribeBroadcast`**, never a per-pod durable group via `Subscribe` — per-pod durables are never cleaned up; the leak wedged JetStream on 2026-07-25 (1039 orphaned consumers, 2m+ CONSUMER.CREATE latency). Broadcast delivery is best-effort (DeliverNew); a missed invalidate = stale cache until the warm-cache poll backstop.
- **At-least-once everywhere → handlers MUST be idempotent.** Durable path: AckWait 30s (60s batch), MaxDeliver 5. Redelivery dedup key = `Nats-Msg-Id` header if set, else stream sequence (`msgID` in `natsbus.go`).
- **Exactly-once = both halves.** Publisher side: stable trace-derived msg IDs (`"win:"+trace`) + the stream's 30m `Duplicates` window — the window must exceed any outage + spool-drain cycle (the old 2m window produced 1,212 duplicate impressions in the 2026-08-05 chaos run). Consumer side: `IdempotentHandler` and/or business-key dedup in reporting.
- **Never add a side-channel fallback for a failed publish** — route through the spool-armed `Publisher`. An "error" publish may have actually reached the stream (ambiguous ack); the tracker's old HTTP fallback double-delivered ~1k impressions per NATS bounce.
- **Boot-latch doctrine:** never close/latch the bus on a boot-race failure. Use `natsbus.EnsureStreamWithRetry` (retries in background until it sticks). The client reconnects forever (`MaxReconnects(-1)` — a capped budget permanently killed the connection after a ~30s outage).
- **Deaf-on-boot doctrine (caller-side):** `Subscribe` can lose the NATS boot race and return an error. Every service consumer MUST retry-until-stick, never log-and-forget — see the retry loop in `cmd/reporting/main.go` (same pattern in webhooks/notifications).
- Spool residual: emptyDir survives container restarts but NOT pod eviction — deliberate, monitored trade-off (`adtech_events_spool_bytes`, `adtech_events_dropped_total`).

## Used By

Every service and job that publishes or consumes events (exchange, tracker, SSP, DSP, reporting, webhooks, notifications, identity-consumer, audience-rt, pipeline, gateway, …). Spool is armed on the loss-critical producers (tracker, exchange, adserver).

## Testing

- Unit tests in-package (`idempotent_test.go`, `spool_test.go`, `payloads_test.go` — enforces `schema_version` is the first JSON field on every payload).
- Use `MemoryBus` (or fakes in `pkg/testutil/`) in unit tests; real NATS via testcontainers for `-tags=integration`. Never mock NATS.

## Pointers

- `docs/PLAN.md` → "NATS Subjects (Async Events)" — the subject/stream catalog; update it when adding a subject.
- `docs/PLAN.md` → "Single Source of Truth: The AuctionWinEvent" — why the win event is the money record.
- `docs/PLAN.md` → "NATS Event Flow (Mermaid)" — update when a subject or producer/consumer edge changes.
- `docs/PLAN.md` → "Build Status & Outstanding Work" → "Event-spool residual" row — the eviction-loss analysis and revisit triggers.
