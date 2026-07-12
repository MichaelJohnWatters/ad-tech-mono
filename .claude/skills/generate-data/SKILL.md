---
name: generate-data
description: Fill the platform with realistic data across every feature — seed (campaigns/creatives/deals/direct-sold/publishers/agencies) + generate attributed traffic through the real serving path (auctions → wins → serve → impressions/views/clicks/conversions → reporting/billing). Use when asked to populate the portals, "run lots of ads", generate demo/load data, or set up a data-rich environment. Covers bounded (few thousand) to millions, continuous streams, and every simulator option.
---

# Generate data: seed + traffic

Two steps: **seed** the entities (once), then **run traffic** to produce
attributed events. Traffic goes through the *real* stack (SSP → exchange → DSP
fan-out → win → serve → tracker → NATS → reporting/billing), so it drives the
same paths production would and fills every portal.

## 0. Prereqs
- Stack up (see the `local-stack` skill if ports refuse). If OrbStack slept,
  recover first.
- For big/long runs, keep the Mac awake: `caffeinate -i -t 3600 &`.

## 1. Seed (all features, once)
The seed loads **every** YAML in `profiles/` regardless of `--profile`:
campaigns across **all formats** (display/native/video/audio + CTV pods),
creatives, targeting, publishers, placements, **deals** (PG/Preferred/PMP),
**direct-sold** line items, advertisers + **agencies**, and loginable accounts.

```
go run ./cmd/seed --profile standard      # or: make seed
```
Or the one-command rich setup (seed + cache refresh + baseline traffic + rollups):
```
make demo
```
**After a manual seed you MUST refresh warm caches** (they self-heal in ~30s, but
a re-seed doesn't push invalidates, and an OrbStack resume can wedge the poll):
```
for p in 8085 8082 8084 8081 8089 8090; do curl -sX POST localhost:$p/debug/cache/refresh; done
```
(8085 adserver=creatives · 8082 dsp=campaigns · 8084 ssp=placements · 8081 exchange=deals)

Logins (password `admin`): `admin@adtech.local` (staff), `advertiser@adtech.local`,
`publisher@adtech.local`. If login says "invalid" → the seed hasn't run.

## 2. Run traffic — it's fully parameterized

`go run ./cmd/simulator run [flags]`

| Flag | What | Example |
|---|---|---|
| `--profile` | `trickle` (1rps, display) · `steady` (10rps, all formats) · `burst` (100rps) | `--profile steady` |
| `--requests N` | **stop after exactly N ads** (the few-thousand→millions dial; 0 = use duration) | `--requests 1000000` |
| `--rps n` | throughput (how fast) | `--rps 500` |
| `--duration d` | time-based instead of a count | `--duration 2h` |
| `--conv-rate f` | P(conversion \| click) — overrides the profile default | `--conv-rate 0.4` |
| `--channel ch` | force one format: display/video/audio/native | `--channel video` |
| `--persona name` | force one persona (`simulator personas`) | `--persona eu-consented-mobile` |
| `--geo` / `--device` | override geo (ISO-3) / device | `--geo USA --device ctv` |
| `--pod n` | request a CTV ad pod of n ads (video) | `--pod 4` |
| `--verify` | after the run, assert impressions==wins vs reporting | `--verify` |

### The funnel (what fires per served ad)
- **Impression** — always (100% of wins)
- **Viewability** — `ViewPct` of impressions
- **Click** — `ClickRate` of impressions (steady = 2%)
- **Conversion** — `ConvRate` of *clicks* (steady = 10%), fired as the advertiser's
  `/v1/t/conv` pixel

So `conversions ≈ impressions × ClickRate × ConvRate`. At realistic rates that's
~0.2% of impressions — **conversions need volume** (or crank `--conv-rate` /
`ClickRate` for a demo).

### Recipes
```
# Fill the portals for a demo (bounded, ~2 min, visible conversions):
go run ./cmd/simulator run --profile steady --requests 5000 --rps 40 --conv-rate 0.4

# A million ads (bounded; ~33 min at 500rps — keep the Mac awake):
caffeinate -i go run ./cmd/simulator run --profile steady --requests 1000000 --rps 500

# Sustained stream (best for very large / long — survives hiccups):
make traffic DEMO_RPS=200        # or the 'sim-continuous' Tilt resource

# One format / a specific segment:
go run ./cmd/simulator run --profile steady --requests 3000 --channel video --geo GBR
```

## 3. Aggregate + verify
Roll up so time-bucketed dashboards fill, then optionally verify losslessness:
```
for lvl in minute hourly daily; do curl -sX POST "localhost:8086/debug/rollup/run?level=$lvl"; done
go run ./cmd/simulator run --profile trickle --requests 200 --verify   # impressions==wins + engine check
```
Sanity counts (ClickHouse HTTP):
```
CH="http://localhost:8123/?user=adtech&password=adtech-local-dev"
for t in impressions clicks views conversions auctions; do
  echo "$t: $(curl -s "$CH" --data-binary "SELECT count() FROM adtech.$t")"; done
```

## Gotchas
- **Big runs stall on OrbStack sleep / port-forward drops** — a bounded
  `--requests 8000` truncated once for this reason. For millions, prefer a
  **continuous stream** (`make traffic` / `sim-continuous`) with `caffeinate`,
  and just let it accumulate; use `--requests` for bounded "give me exactly N".
- **Re-seed → refresh caches** (step 1) or the portals look empty/stale.
- Traffic hits the simulator publisher's placements; `impressions.format` and
  auction `deal_id` may come through empty on this path (known gaps, not volume).
