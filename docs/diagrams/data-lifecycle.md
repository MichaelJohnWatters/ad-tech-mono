# Data lifecycle — one linear walk from data-in to audience + rollups

The other diagrams each show *one segment* of the data story
([`auction-flow`](auction-flow.svg) = serving, [`nats-events`](nats-events.svg) =
fan-out, [`data-reporting`](data-reporting.md) = hot/cold, [`data-pipeline`](data-pipeline.svg)
= the lake, [`targeting-data-flow`](targeting-data-flow.svg) = audience). This
page is the **single linear thread** that connects them: where every byte comes
from, how the auction produces it, how it becomes an audience, and how it rolls
up into dashboard numbers.

Read it top-to-bottom. There is **one input funnel**, then the stream **forks
into two spines** that never touch each other except through four small feedback
loops (marked ⟲).

```
╔══════════════════════════════════════════════════════════════════════════════╗
║  STAGE 1 — WHERE DATA COMES FROM   (two lanes: you don't control who, only how)║
╚══════════════════════════════════════════════════════════════════════════════╝

  LANE A · OBSERVED (exhaust)                  LANE B · ONBOARDED (1st + 3rd party)
  the platform watches things happen           someone hands us a list of people
  ───────────────────────────────             ────────────────────────────────────
  Browser / CTV device                         Advertiser / Publisher      Data partner
        │ loads adtech.js                             │ CSV/TSV upload            │ drops a file
        │  + pixels                                   │ (PII hashed in browser)   │ csv/tsv/parquet
        ▼                                             ▼                           ▼
  ┌───────────────┐   ┌──────────────────┐    ┌────────────────────┐   ┌──────────────────────┐
  │ Ad request    │   │ Tracker :8083    │    │ Gateway            │   │ Minio drop-zone      │
  │ → SSP :8084   │   │ imp/click/       │    │ /v1/api/audiences  │   │ adtech-onboarding/   │
  │ (user_id,uid2,│   │ conv/view        │    │ (match rate/upload)│   │  <provider>/incoming │
  │ hashed_email, │   │                  │    └─────────┬──────────┘   └──────────┬───────────┘
  │ ifa, IP→hh,   │   │ Retargeting      │              │                         │ Pipeline poller:
  │ page context) │   │ pixel /v1/t/rt   │              │                         │ decode→validate→
  └──────┬────────┘   │ → site_visit rows│              │                         │ normalise→quarantine
         │            └───────┬──────────┘              │                         │
         │                    │                         │                         │
         └──── these two feed THE AUCTION (stage 2) ─────┘   both onboarded paths ─┘
                    and emit events (stage 3)                land in stage 4 (lake)
                                                             + write segments directly (stage 5)

╔══════════════════════════════════════════════════════════════════════════════╗
║  STAGE 2 — THE AUCTION  (the "observed" lane is really the exhaust of THIS)     ║
╚══════════════════════════════════════════════════════════════════════════════╝

   SSP :8084 ──▶ Exchange ──▶ DSP :8082 ──▶ (winner) ──▶ Ad Server ──▶ Browser renders
   builds bid    fan-out to    each DSP       first-price   returns the    the creative
   request,      DSPs, deal    bids; unions   auction +     creative +     │
   stamps        priority      audience bid   bid shading   tracking URLs  │ user sees / clicks
   user.ext      (PG>Pref>     modifiers      + floor                      │
   .segments     PMP>Open),    into price                                  ▼
   ⟲(from        floor                                          Tracker :8083 records
   Redis, 25ms)  enforced                                       impression / click /
                                                                conversion / viewability
                                                                        │
        every step writes a row with the SAME trace_id ────────────────┘
        (SSP-minted, W3C 32-hex; flows through logs, NATS, ledger, lake)

╔══════════════════════════════════════════════════════════════════════════════╗
║  STAGE 3 — THE EVENT BUS  (one stream in, many independent readers out)         ║
╚══════════════════════════════════════════════════════════════════════════════╝

                        Tracker + Exchange + SSP  publish protobuf events
                                          │
                                          ▼
                        ╔═════════════════════════════════╗
                        ║   NATS JetStream (durable)       ║
                        ║   adtech.* subjects              ║
                        ╚═════════════════════════════════╝
        each consumer group gets its OWN copy (fan-out, not load-balanced):
   ┌───────────────┬──────────────────┬────────────────┬───────────────┬──────────────┐
   ▼               ▼                  ▼                ▼               ▼              ▼
 reporting       pipeline           billing         identity-       webhooks     notifications
 group           group              group           consumer        group        group
 (→ HOT +        (→ COLD lake)      (→ TigerBeetle   group           (HTTP POST)  (bell rows)
  rollups)                           ledger)         (→ id graph)
   │               │                                    │
   │ STAGE 6       │ STAGE 4                             │ STAGE 5 (identity spine)
   ▼               ▼                                     ▼

╔══════════════════════════════════════════════════════════════════════════════╗
║  STAGE 4 — THE LAKE  (cold store: every event as a replayable Parquet fact)     ║
╚══════════════════════════════════════════════════════════════════════════════╝

   Pipeline :8087 datalake sink                       Delta lake (Minio / S3)
   ┌──────────────────────────────┐    writes     ┌──────────────────────────────────┐
   │ per-subject handler           │──────────────▶│ event_date=YYYY-MM-DD/            │
   │  → per-table buffer           │  Parquet +    │   part-NNNNN.parquet  (7 event    │
   │  → ACK ONLY after durable      │  Delta commit │   tables + behaviour_signals +    │
   │    flush (at-least-once)       │               │   profile_signals + id_clusters)  │
   └──────────────────────────────┘               │ _delta_log/  (real Delta protocol)│
        ▲                                          └──────────────────────────────────┘
        │ onboarded rows (stage 1 lane B)            single writer = pipeline, always:
        │ also land here as profile_signals          · compaction (bin-pack, via pipeline)
        └────────────────────────────────           · GDPR purge (filtered rewrite)
                                                     · keep-forever (ML training corpus)

╔══════════════════════════════════════════════════════════════════════════════╗
║  STAGE 5 — BECOMING AN AUDIENCE  (where the two source lanes finally MEET)      ║
╚══════════════════════════════════════════════════════════════════════════════╝

   IDENTITY SPINE (who is the same person / household)
   NATS ─▶ identity-consumer ─▶ identity_graph (PG edges, dedup upsert)
   onboarded /v1/api/identity-links ─▶ crm_match edges ──────────────┘
                                                       │
                                                       ▼
   ┌────────────────────────────────────────────────────────────────────────────┐
   │ profile-builder   (a step in the batch-conductor chain, runs on a schedule)  │
   │   reads: identity_graph (PG)  +  behaviour_signals & profile_signals (lake)  │
   │   ①  cluster graph → person_id            (union-find over the id edges)      │
   │   ②  behavioural rules (pure-Go over the Delta reader) + reconcile windows    │
   │   ③  expand: enroll the person, fan out to EVERY id in their cluster          │
   └───────────────────────────────┬────────────────────────────────────────────┘
                                    │ writes
                                    ▼
   audience_segment_members (PG)  ──▶ audience preloader ──▶ Redis
                                       (interval bulk push)   audience:user:<id>:<vis>
                                    │                              │
   ⟲ FEEDBACK LOOP: publishes      │                              │ 25ms hot-path read
     cache.invalidate.audience     │                              ▼
     so the loop below refreshes    │                    back into STAGE 2:
                                    │                    · SSP stamps user.ext.segments
   OUTPUTS:                         │                    · DSP applies audience bid modifiers
   · Segment exports (CSV/Parquet via report jobs)
   · Profile API  GET /v1/api/profiles/<id>  (staff, support:read)
   NOTE: onboarded uploads (stage 1 lane B) can ALSO write segment members
         directly — the live "fast path" — while the lake copy feeds the builder.

╔══════════════════════════════════════════════════════════════════════════════╗
║  STAGE 6 — ROLLUPS  (the HOT spine: raw events → ever-coarser summaries)        ║
╚══════════════════════════════════════════════════════════════════════════════╝

   Reporting :8086 NATS consumer ──▶ ClickHouse (raw events)
                                          │  each tier aggregates the previous and
                                          │  the daily/monthly jobs PURGE the finer one
                                          ▼
   RAW events ──min──▶ MINUTE ──hour──▶ HOURLY ──day──▶ DAILY ──month──▶ MONTHLY
   group by: trace_id  campaign/creative/placement/geo/device            (+ ROAS)
   retain:   24–48h    7 days          90 days         2 years           forever
   run by:   —         rollup-minute   rollup-hourly   rollup-daily      rollup-monthly
             (raw)     (every min)     (every hour)    (00:30 UTC,       (1st @01:00,
                                                        purges raw>48h    purges
                                                        + minute>7d)      hourly>90d)

   READ PATH  (portal asks a question):
   Portal ─▶ Gateway (enforceReportTenant: injects account/publisher scope)
          ─▶ QueryEngine (computes eCPM/CTR/fill_rate/net_revenue SERVER-SIDE)
          ─▶ AutoTier (picks the coarsest tier that answers the range)
          ─▶ HotColdStore (routes by age vs hot_window):
                recent  → ClickHouse (HOT)
                aged    → Delta lake  (COLD, DuckDB delta_scan)   ← stage 4
                spanning→ split + additive merge of both
          ─▶ rows back to the portal (no browser math)
```

## The same flow, rendered (Mermaid — for GitHub)

The ASCII above is the terminal view; this is the identical story as a rendered
flowchart. Solid arrows = sync/data flow, dotted = async or feedback loop.

```mermaid
flowchart TD
  %% ---------- ① sources ----------
  BR["Browser / CTV<br/>runs adtech.js + pixels"]
  OWN["Advertiser / Publisher<br/>CSV upload"]
  PART["Data partner<br/>drops a file"]

  %% ---------- ② auction ----------
  SSP["SSP 8084<br/>bid request · stamps segments"]
  EX["Exchange<br/>fan-out · deal priority · floor"]
  DSP["DSP 8082<br/>bids + audience modifiers"]
  ADS["Ad Server<br/>serves creative + signed tracking URLs"]
  TRK["Tracker 8083<br/>records the 7 beacons"]

  BR -->|"ad request"| SSP
  SSP --> EX --> DSP -->|"winner"| ADS -->|"renders"| BR
  ADS -.->|"imp click view video audio (signed)"| TRK
  BR -.->|"conv · rt pixels on advertiser site"| TRK

  %% onboarded lane
  OWN -->|"/v1/api/audiences"| GW1["Gateway · audiences"]
  PART --> DZ["Minio drop-zone"] --> POLL["Pipeline poller<br/>decode · validate · normalise"]

  %% ---------- ③ NATS fork ----------
  SSP --> NATS
  EX --> NATS
  TRK --> NATS
  NATS{{"③ NATS JetStream · adtech.*<br/>fan-out: each group its own copy"}}

  NATS -->|"reporting group"| CH[("ClickHouse · HOT")]
  NATS -->|"pipeline group"| SINK["Pipeline sink 8087<br/>ack after flush"]
  NATS -->|"billing group"| TB[("TigerBeetle ledger")]
  NATS -->|"identity group"| IDC["identity-consumer"]

  %% ---------- ④ cold lake ----------
  SINK --> LAKE[("④ Delta lake (Minio)<br/>Parquet + _delta_log")]
  POLL --> LAKE
  GW1 -.->|"lake copy"| LAKE

  %% ---------- ⑤ audience ----------
  IDC --> IDG[("identity_graph · PG edges")]
  subgraph PB["⑤ profile-builder · batch"]
    direction LR
    C1["1 cluster to person_id"] --> C2["2 behavioural rules"] --> C3["3 expand to cluster"]
  end
  IDG --> PB
  LAKE -.->|"behaviour + profile signals"| PB
  PB --> MEM[("audience_segment_members · PG")]
  GW1 -->|"direct fast path"| MEM
  MEM --> PRE["audience preloader"] --> RDS[("Redis · audience:user:id")]
  PB -.->|"cache.invalidate.audience"| PRE
  RDS -.->|"25ms · feedback loop"| SSP
  RDS -.->|"bid modifiers"| DSP
  MEM --> EXP["Segment exports · Profile API"]

  %% ---------- ⑥ rollups + read ----------
  subgraph ROLL["⑥ Rollups · ClickHouse tiers (coarser = cheaper = kept longer)"]
    direction LR
    RAW["RAW<br/>24-48h"] -->|"minute"| MIN["MINUTE<br/>7d"] -->|"hourly"| HR["HOURLY<br/>90d"] -->|"daily"| DAY["DAILY<br/>2y"] -->|"monthly"| MON["MONTHLY<br/>forever"]
  end
  CH --> RAW

  PORTAL["Portal"] -->|"/v1/api/reports"| GW2["Gateway<br/>enforceReportTenant"]
  GW2 --> QE["QueryEngine<br/>eCPM · CTR · fill · net_rev"]
  QE --> AT["AutoTier · pick coarsest tier"] --> HCS{{"HotColdStore · route by age"}}
  HCS -->|"recent"| CH
  HCS -.->|"aged · delta_scan"| LAKE
  ROLL --> HCS
  HCS --> QE
  QE -->|"rows"| PORTAL

  classDef hot fill:#fde68a,stroke:#b45309;
  classDef cold fill:#faf5ff,stroke:#7c3aed;
  classDef bus fill:#dcfce7,stroke:#15803d;
  class CH,RAW,MIN,HR,DAY,MON hot;
  class LAKE,SINK cold;
  class NATS bus;
```

## The pixels — who fires what, and what it triggers

"The tracker" in stage 1/2 is really **seven beacons** (`/v1/t/*` on the Tracker
:8083). They split into two families by *who places the pixel*, and each one
triggers a different downstream effect. Every ad-served beacon is **HMAC-signed**
by the ad server; a tampered beacon is `403`'d when `tracker.signature_validation`
is on.

**Family 1 — ad-server-emitted** (baked into the served creative's tracking URLs,
signed; these measure the ad and drive the money loop):

| Pixel | Endpoint | Fires when… | Triggers downstream |
|---|---|---|---|
| Impression | `/v1/t/imp` | the ad renders | **billing accrual** — spend is booked *here*, not on the auction win |
| Click | `/v1/t/click` | user clicks (redirect tracker) | **CPC settle** + click row |
| Viewability | `/v1/t/view` | IAB viewable threshold met | **vCPM settle** (viewable→settle; non-viewable reservation expires) |
| Video | `/v1/t/video` | VAST quartiles / completion | video engagement rows (**CPCV** settle on complete) |
| Audio | `/v1/t/audio` | DAAST audio events | audio engagement rows |

**Family 2 — advertiser-placed** (the advertiser embeds these on their *own*
site; the gateway generates the snippet so the URL is never hardcoded):

| Pixel | Endpoint | Placed on… | Triggers downstream |
|---|---|---|---|
| Conversion | `/v1/t/conv` | advertiser thank-you / purchase page | **CPA settle** + conversion attribution (matched back by `trace_id`) |
| Retargeting | `/v1/t/rt` | advertiser product / category pages | **`site_visit` rows** → feed audience segments (this is how "visited but didn't buy" becomes a targetable list — stage 5) |

**How `/v1/t/rt` actually becomes a segment** (the bit the table glosses): the
retargeting pixel doesn't write "segment members" directly. It publishes a
**consent-gated `site_visit` behaviour row** (`kind=site_visit`, tagged with the
advertiser's own label, e.g. `product-page`) — the *same* `behaviour_signals`
lane as every other observed behaviour. It becomes a segment only when a
**behavioural rule** with `event=site_visit` + a tag filter matches it in
profile-builder (stage 5). So "visited /pricing but didn't buy" is just a rule
over site-visit rows, then cluster-expanded like any other segment. No consent →
no row → no retargeting.

**The pixel that isn't a pixel:** identity/household signals (`user_id`, `uid2`,
`hashed_email`, `ifa`, `IP→household`) don't ride a separate sync pixel — they
arrive on the **ad request itself** at the SSP (stage 2), then flow to the
identity graph. So "how do we know who this is" and "did the ad work" enter
through *different* doors.

Every one of these seven, once recorded, publishes to NATS (stage 3) and so
lands in **both** the hot rollups (stage 6) and the cold lake (stage 4) — plus
billing (settles) and, for `/v1/t/rt`, the audience spine (stage 5).

## Zoom-in A — how the pipeline NORMALISES onboarded data (stage 4 detail)

This is the part that's easy to hand-wave. A CSV from an advertiser and a
Parquet from a data partner and a subscriber list from a publisher must all end
up as the **same shape**: one row per identifier, with a hashed id, an id-type,
who it belongs to, and consent. That flattening is the pipeline's whole job.

There are **two doors in**, both ending at the same `profile_signals` shape:

```
DOOR 1 · Portal upload (small, interactive)      DOOR 2 · Drop-zone (bulk, partner)
advertiser OR publisher picks a CSV               partner drops a file + a manifest
        │ browser hashes PII (SHA-256)                    │ adtech-onboarding/<provider>/
        │ raw emails REJECTED, ≤5 MB                      │   incoming/  (csv·tsv·parquet,
        ▼                                                 │   zip/gzip auto-detected)
  POST /v1/api/audiences  ─────────┐                      ▼
  (Gateway, tenant-scoped to the   │            ┌──────────────────────────────────────┐
   caller's account — never        │            │ Pipeline poller (cmd/pipeline)         │
   trusted from the body)          │            │                                        │
        │                          │            │  ① DECODE   sniff magic bytes →        │
        ▼                          │            │             rows (csv/tsv/parquet)     │
   ┌──────────────────────┐        │            │  ② MAP      manifest.json field_map:   │
   │ normalise inline:     │       │            │             "email"→id_value,          │
   │  id columns → id_value│       └───────────▶│             "provider_uid"→id_value,   │
   │  + id_type            │                     │            id_type stamped (defaults    │
   │  match rate computed  │                     │            merged UNDER the manifest)  │
   └──────────┬───────────┘                      │  ③ VALIDATE required fields present?   │
              │                                   │             NO → quarantine row to     │
              │                                   │             <provider>/rejected/ +     │
              │                                   │             an onboarding_runs record  │
              │                                   │  ④ ENRICH   geo from IP, device class  │
              │                                   │  ⑤ MATCH    % of ids already known to   │
              │                                   │             the identity graph =        │
              │                                   │             "match rate" shown to       │
              │                                   │             the uploader                │
              │                                   └───────────────────┬────────────────────┘
              │                                                       │
              ▼  writes BOTH:                                         ▼  writes BOTH:
   ┌─────────────────────────────────────────────────────────────────────────────────┐
   │ profile_signals (Delta lake)     ← durable, replayable: 1 row per id              │
   │   {id_type, id_value(hashed), source, account_id, provider, segment, consent, ts} │
   │ audience_segment_members (PG)    ← the live "fast path" — usable in auctions NOW   │
   └─────────────────────────────────────────────────────────────────────────────────┘
```

**Why two writes?** The PG row makes the list usable in the *next auction*
immediately (the fast path). The lake row is the durable fact the
**profile-builder** re-reads every run to expand + reconcile (below). Same data,
two speeds.

## Zoom-in B — how audiences EXPAND (the part that feels like magic)

An upload only ever contains the ids the customer *had* (say, hashed emails). But
the same person also has a cookie, a mobile ad id, a household. **Expansion** is:
"enrol the person once, then apply that membership to *every* id we know is the
same person." That's what turns a 10k-email list into a segment that actually
matches traffic (which arrives as cookies/IFAs, not emails).

```
profile-builder  (a step in the hourly batch-conductor chain, pkg/profilebuilder)

  INPUT ─ identity_graph (PG edges)         INPUT ─ behaviour_signals + profile_signals (lake)
          who is linked to whom                     what each person did / was uploaded as
              │                                              │
              ▼                                              │
  ① CLUSTER  union-find over the edges:                      │
     • keep edges with confidence ≥ 0.5                      │
     • exclude household (hh:) links                         │
     • drop pathological mega-clusters                       │
     → person_id  ⇄  [all member ids]                        │
              │                                              │
              ▼                                              ▼
  ② SEGMENT  build membership at the PERSON level, four kinds:
     • onboarded (plain)   the uploaded list, as-is
     • behavioural rule    "visited /pricing 3× in 7d"  (evaluated over behaviour_signals)
     • composite           all_of / any_of / none_of  (boolean set logic on other segments)
     • lookalike           seed segment → top-K categories → score every other person →
                           enrol those ≥ min_similarity, capped at max_members
              │
              ▼
  ③ EXPAND   expandKeys(): for each enrolled person, fan the membership out to
             EVERY id in their cluster.
             ┌──────────────────────────────────────────────────────────────┐
             │  enrolled:  sha256(email)  ─ in segment "newsletter"          │
             │  cluster of that person also holds:  cookie:xyz, ifa:123, ... │
             │  after expand:  cookie:xyz, ifa:123  ALSO in "newsletter"     │
             └──────────────────────────────────────────────────────────────┘
              │
              ▼
  ④ WRITE    audience_segment_members (PG, replace-by-segment prune) + lake durable copy
              │   ⟲ publishes cache.invalidate.audience
              ▼
  audience preloader → Redis (audience:user:<id>) → back into the AUCTION (stage 2):
     SSP stamps user.ext.segments · DSP applies the audience bid modifier
     (+ optional read-time BFS top-up on the DSP for extra freshness)
```

### Worked example — follow one person end-to-end

1. A **publisher** uploads `subscribers.csv` (column `email`) as segment
   "newsletter", visibility `public`. The browser hashes each email to
   `sha256:ab…`; the raw email never leaves the page.
2. Pipeline normalises → `profile_signals` row `{id_type:email, id_value:sha256:ab…,
   account:pub-42, segment:newsletter, consent:yes}` **and** a PG member row.
   Match rate comes back "68% of these emails are already known to us."
3. The **identity graph** already links `sha256:ab…` ⇄ `cookie:xyz` (seen in a
   past auction) ⇄ `hh:home7` (same IP household).
4. **profile-builder** clusters those three ids into `person_42`, enrols
   `person_42` in "newsletter", then **expands**: now `cookie:xyz` and `ifa:…`
   are members too — not just the email.
5. Members → Redis. The **next auction** where `cookie:xyz` shows up: the SSP
   stamps `segments:[newsletter]`, the DSP sees it and applies the newsletter bid
   modifier. The email became a cookie became a bid.

**That is the whole pipeline.** Normalise (make every source one shape) →
cluster (who's the same person) → expand (apply the label to all their ids) →
serve (target them in the auction).

## Publisher-submitted data — yes, and it's symmetric

Onboarding is **not advertiser-only**. Publishers onboard their own first-party
data through the *same* two doors, tenant-scoped to the publisher account:

- **Publisher portal → "First-party audiences"** (subscriber lists, behavioural
  cohorts, suppression lists) via `POST /v1/api/audiences` — identical to the
  advertiser path, just a publisher session.
- **Drop-zone** for a publisher's data partner, same manifest ingestion.

The supply-side use is different (a publisher's segments enrich *their own*
inventory / power seller-defined audiences) but the **plumbing is the same**:
normalise → cluster → expand → available for targeting. In the diagrams the
onboarded lane says "Advertiser / **Publisher**" for exactly this reason.

## Cross-cutting — consent & the right to be forgotten (the flow that runs BACKWARDS)

Every stage above moves data *forward*. There is one flow that runs the other
way — the privacy path — and it touches all three stores. `CLAUDE.md` requires
consent to flow through the entire chain; here's where it actually lives.

```
CONSENT is enforced at CAPTURE, not later:
  · no consent on the ad request → identity signals not observed
  · no consent on a serve       → tracker emits NO behaviour_signals row
    (publishBehaviour is consent-gated) → nothing to onboard, cluster or retarget

THE RIGHT TO BE FORGOTTEN (opt-out → deletion), a 3-level system:
  User / regulator
      │  POST /v1/api/privacy/optout
      │  level 1 = no personalisation (contextual only, still bids) ·
      │  level 2 = no tracking (no bid) · level 3 = full deletion
      ▼
  opt_out_registry (PG)  ──publishes──▶  adtech.privacy.opt_out  (serving stops targeting NOW)
      │
      │  level 3 → cmd/privacy-delete (job) picks up rows not yet completed
      ▼
  ┌───────────────────────────────────────────────────────────────────────────┐
  │ PURGE the user across ALL THREE stores in one sweep:                        │
  │   · identity_graph            — drop their edges (un-links the person)      │
  │   · audience_segment_members  — drop their memberships (leaves every segment)│
  │   · Delta lake (via pipeline) — filtered rewrite drops their event rows     │
  │     (the lake is keep-forever EXCEPT this — GDPR beats "keep everything")   │
  └───────────────────────────────┬───────────────────────────────────────────┘
      │  marks opt_out_registry completed · publishes adtech.privacy.deletion_completed
      ▼
  cmd/privacy-verify (audit) — re-scans the stores; a leak = the delete didn't propagate
```

The lake being the pipeline's **single writer** is what makes the purge safe: one
process rewrites the Parquet + commits one Delta version, so a scan never sees a
half-deleted user.

## Analytics has readers beyond the dashboard

Stage 6 shows the portal reading rollups, but the hot/cold data has **other
consumers** — the analytics spine isn't just for humans:

- **`cmd/optimise`** — bid / placement / creative optimisation reads back
  performance + the `dsp_call` auction-log events to tune routing and pacing.
- **`cmd/fraud`** + **`python/fraud`** — batch fraud scoring over the event/auction
  history; blocklist output feeds back to the tracker (warm cache).
- **`python/optimisation`** — model training on the keep-forever Parquet corpus
  (this is *why* the lake keeps everything: Parquet is the ML-native format).
- **⟲ spend → pacing** — reporting computes committed spend, snapshots it, and the
  DSP reconciles its pacing counter against it (one of the four analytics→serving
  feedback loops; see [`billing-flow`](billing-flow.svg) and [`cache-freshness`](cache-freshness.svg)).

## The one-sentence version of each stage

1. **Sources** — two lanes: *observed* (pixels + auction exhaust) and *onboarded*
   (1st/3rd-party lists via upload or a Minio drop-zone). You control *how* data
   arrives, never *who* it's about.
2. **Auction** — SSP→Exchange→DSP→win→AdServer→Tracker. The "observed" lane is
   literally the exhaust of this pipe; one `trace_id` stitches every row.
3. **Event bus** — one durable NATS stream; each consumer group (reporting,
   pipeline, billing, identity, webhooks, notifications) gets its **own copy** and
   goes its own way. This is the fork.
4. **Lake (cold)** — the pipeline sink writes every event as Parquet + a real
   Delta log, ack-only-after-flush. It is the single writer and the keep-forever
   ML corpus. Onboarded rows land here too, normalised to `profile_signals`.
5. **Audience** — identity graph says *who is the same person*; **profile-builder**
   clusters → applies rules → expands membership to every id in the cluster,
   writes `audience_segment_members`, and pushes them to Redis so the **auction in
   stage 2** can target them (a feedback loop, ≤30s fresh).
6. **Rollups (hot)** — ClickHouse raw events cascade raw→minute→hourly→daily→
   monthly; each coarser tier is cheaper and kept longer, and the coarser job
   purges the finer one. The reporting engine auto-picks the right tier and
   merges hot+cold behind one query.

## The four things that trip people up

- **Two stores, one stream — not a mover.** Reporting writes ClickHouse (hot),
  pipeline writes the lake (cold), *from the same events* via independent NATS
  groups. There is **no** scheduled hot→cold copy; `hot_window` is only a
  read-routing line. (ClickHouse also keeps a 30-day TTL as a safety net.)
- **Rollups are an optimisation, never the source of truth.** `AutoTier` serves
  a pre-aggregated tier when it can and falls back to raw otherwise —
  correctness never depends on a rollup having run.
- **The audience spine and the rollup spine are separate circulatory systems.**
  Serving/audience caches feed from Postgres + NATS invalidates; the analytics
  spine (ClickHouse/lake/rollups) is **read-side only**, except four deliberate
  feedback loops (⟲): audience memberships, spend→pacing reconcile, boot
  warm-starts, and identity top-up. See [`cache-freshness`](cache-freshness.svg).
- **The batch-conductor is the conductor, not a step.** It runs the whole cold
  chain in completion order every hour: `checkpoint → compact → rollups →
  profile-builder → privacy purge → verify`. Stages 4, 5, and 6's cold half all
  hang off it.
