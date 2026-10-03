# Audience-RT Service

Real-time retargeting consumer (:8097). Turns a shopper's `site_visit` into
audience membership within seconds (vs the hourly batch profile-builder) and
suppresses the chase the moment they purchase. Off the serving path entirely:
it only changes membership *latency*; the DSP's bid-time audience code is untouched.

## Responsibilities

- Consume behaviour signals; on a `site_visit`, enroll the visitor into the
  advertiser's matching retargeting segment(s) (`pkg/retargeting.OnSiteVisit`)
- Consume conversions; a `purchase` suppresses the converter (burn list,
  migration 082, expanded person+household via the identity graph)
- Optional household enrollment (`audience_rt.household_enroll`, live key):
  the `hh:` salted-IP id enrolls alongside the visitor id
- Dynamic Product Ads: SKU views → `retargeting_product_views`; per-product
  suppression + catalog cross-sell on SKU-carrying purchases
- Publish a `RetargetingEnrolledEvent` per enrollment (abandoned-cart webhooks)
- Storage hygiene: ticker purge of expired members/suppressions/product views
  (`audience_rt.purge_interval`; read paths already exclude expired rows)

## Interfaces

HTTP (ops only — no business endpoints): `/healthz`, `/readyz`, `/metrics`.

NATS consumed (queue group `constants.NATSGroupAudienceRT`):
- `adtech.behaviour.observed` — consent-gated site visits (SSP + tracker `/v1/t/rt`)
- `adtech.events.conversion` — only `conversion_type=purchase` suppresses

NATS published:
- `adtech.retargeting.enrolled` — account-scoped, consumed by webhooks

## Key Packages Used

- `pkg/retargeting/` - the pure core (rule match, enroll/suppress, TTLs)
- `pkg/audience/store/postgres/` - segments, members, suppressions, product views
- `pkg/catalog/postgres/` - product complements for cross-sell
- `pkg/events/natsbus/` - subscribe/publish

## CRITICAL Invariants

- **Single-visit rules only** (`min_count <= 1`). Visit-frequency rules
  (`min_count > 1`) stay with the batch profile-builder — this service has no
  visit history.
- **No cache invalidation from here.** `InvalidateAudience` is a deliberate
  no-op: the `audience_segment_members` trigger (migration 078) appends to the
  change-log in the same transaction, and the pipeline's SINGLE drainer applies
  it to Redis within seconds. Never re-add invalidate broadcasts.
- **Replicas:** helm runs 2 (HA). Safe multi-replica: enrollment is an
  idempotent upsert, events are queue-group consumed, purge is an idempotent
  DELETE. (Older "run 1 replica" comments predate this.)
- **Poison vs transient:** malformed payload → Ack (drop, no redelivery);
  store error → Nak (JetStream redelivers). Keep handlers idempotent.
- **Boot-retry doctrine:** a failed Subscribe at boot retries every 15s until
  it sticks — never latch a dead consumer (see project NATS deaf-on-boot rule).
- Enrollment lineage (migration 080): first enroll records the visit's
  `origin_trace`; repeat visits refresh `expires_at` only.

## Dependencies

- Postgres (audience store; small pool, max 4 conns)
- NATS JetStream (readiness fails without it — no silent degradation)

## Architecture Details

See `docs/AUDIENCE-PIPELINE.md` and `docs/PLAN.md` -> "Real-Time Retargeting
(Built)", "Unified Audience Management Layer", "NATS Subjects".

## Diagram Updates

If you change this service, check if diagrams need updating:
- **New NATS subject consumed/published?** Update `docs/PLAN.md` -> NATS Subjects table + NATS Event Flow diagram
- **New dependency?** Update `docs/diagrams/architecture.d2` and run `make diagrams`
- **Changed the enroll→Redis path (trigger/drainer)?** Check the audience pipeline diagram referenced in `docs/diagrams/README.md`
- **C4 model:** update this service's `component` block + `component <id>` view in `docs/diagrams/workspace.dsl` if you add/remove/rename a component or change a dependency. Keep ids service-prefixed and the DSL valid.
