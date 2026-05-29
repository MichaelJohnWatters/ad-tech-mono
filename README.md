# Ad Tech Mono

A full-stack programmatic advertising platform in a single Go monorepo. Every component - from bid request to impression tracking to analytics - runs locally on K8s (Colima + k3s). The goal is full transparency: trace any ad request end-to-end with zero data slippage.

## Architecture

```
Browser/Publisher -> Gateway (:8080) -> Exchange (:8081) -> DSP (:8082)
                                                         -> Ad Server (:8085)
                                        Tracker (:8083) -> NATS JetStream
                                                         -> Reporting (:8086)

Infrastructure (K8s): Postgres | NATS (3-node) | Redis | Minio (S3)
```

**8 services**, **20 packages**, **130+ tests**, all in Go.

## Prerequisites

- Go 1.23+
- Docker
- Colima (`brew install colima`)
- kubectl (`brew install kubectl`)
- Tilt (`brew install tilt`)

Or run the setup script:

```bash
./scripts/setup.sh
```

## Quick Start

### 1. Start Everything

```bash
tilt up
```

This starts all K8s infra (Postgres, NATS 3-node cluster, Redis, Minio) and builds + runs all 8 services. Open http://localhost:10350 for the Tilt dashboard.

Lite mode (no observability stack):

```bash
PROFILE=lite tilt up
```

Container mode (Docker builds into K8s, same as CI/staging/prod):

```bash
DEV_MODE=container tilt up
```

### 2. Run Your First Auction

```bash
go run ./cmd/simulator single --geo GBR --device mobile
```

This sends a bid request to the Exchange, which fans out to the DSP, runs a first-price auction, and returns the winner. You'll see structured JSON logs from every service.

### 3. Check Services Are Healthy

```bash
curl http://localhost:8080/healthz   # gateway
curl http://localhost:8081/healthz   # exchange
curl http://localhost:8082/healthz   # dsp
curl http://localhost:8083/healthz   # tracker
curl http://localhost:8086/healthz   # reporting
```

## Services

| Service | Port | What it does |
|---|---|---|
| **Gateway** | 8080 | Dashboard, publisher simulator, trace explorer, API entry point |
| **Exchange** | 8081 | Receives bid requests, fans out to DSPs, runs first-price auctions |
| **DSP** | 8082 | Evaluates bids using targeting (11 dimensions) + pacing + bid modifiers |
| **Tracker** | 8083 | Impression/click/conversion/viewability pixels (browser-facing) |
| **SSP** | 8084 | Publisher inventory management |
| **Ad Server** | 8085 | Creative serving |
| **Reporting** | 8086 | NATS event consumer, analytics store, query API |
| **Pipeline** | 8087 | Data pipeline: ingest, validate, normalise, enrich publisher files |

## Developer Tools

### Publisher Simulator

http://localhost:8080/dev/publisher-simulator

A simulated publisher page that runs real auctions. You'll see:
- The ad rendered on a fake news page
- A debug overlay showing trace ID, auction result, pixel firing status
- Links to trace each request

### Trace Explorer

http://localhost:8080/dev/trace-explorer

Trace a single ad request through the entire system. Two ways to use it:

1. **Paste a trace ID** from your logs and click "Trace"
2. **Click "Fire Single Request"** to run a live auction and see every hop

Shows the full flow with timing: Exchange -> DSP -> Ad Server -> Browser -> Tracker -> NATS -> Reporting -> Billing.

### Grafana Dashboards

http://localhost:3000 (when observability stack is running)

Three pre-built dashboards:
- **Pipeline Health** - auction rate, latency, bid rate, fill rate, NATS lag, error rate, revenue
- **Service Detail** - per-service request rate, latency percentiles, memory, goroutines, logs
- **Trace Explorer** - Jaeger span waterfall + Loki log timeline for a given trace ID

### Tilt Dashboard

http://localhost:10350

Shows build status, logs, and health for all services. Manual trigger buttons for:

| Button | What it does |
|---|---|
| seed-minimal | Load minimal seed data |
| seed-standard | Load standard seed data |
| migrate | Run database migrations |
| reset | Reset DB + migrate + seed |
| sim-single | Fire one auction |
| sim-trickle | 1 req/sec for 2 min |
| sim-steady | 10 req/sec for 5 min |
| sim-burst | 100 req/sec for 1 min |
| chaos-kill-redis | Kill Redis pod (test resilience) |
| chaos-kill-nats | Kill NATS pod (test resilience) |
| test-unit | Run `go test ./pkg/...` |
| test-e2e | Run e2e smoke test |

## Traffic Simulation

```bash
# See available profiles
go run ./cmd/simulator profiles

# Single auction with specific targeting
go run ./cmd/simulator single --geo GBR --device mobile

# Trickle: 1 request/sec for 2 minutes
go run ./cmd/simulator run --profile trickle --duration 2m

# Steady: 10 requests/sec for 5 minutes
go run ./cmd/simulator run --profile steady --duration 5m

# Burst: 100 requests/sec for 1 minute
go run ./cmd/simulator run --profile burst --duration 1m

# Check if services are ready before simulating
go run ./cmd/simulator check
```

Each request goes through the full flow: auction -> bid -> win -> impression -> viewability -> maybe click.

## Querying the Analytics Store

The reporting service accepts queries via HTTP POST:

```bash
# Count all impressions
curl -s http://localhost:8086/v1/reporting/query \
  -H 'Content-Type: application/json' \
  -d '{"table":"impressions","metrics":["count","sum_cost"]}' | jq

# Impressions grouped by campaign
curl -s http://localhost:8086/v1/reporting/query \
  -H 'Content-Type: application/json' \
  -d '{
    "table": "impressions",
    "metrics": ["count", "sum_cost"],
    "dimensions": ["campaign_id"]
  }' | jq

# Impressions by geo with time filter
curl -s http://localhost:8086/v1/reporting/query \
  -H 'Content-Type: application/json' \
  -d '{
    "table": "impressions",
    "metrics": ["count", "sum_cost"],
    "dimensions": ["geo"],
    "filters": {"account_id": "acc-123"},
    "time_from": "2024-06-15T00:00:00Z",
    "time_to": "2024-06-16T00:00:00Z"
  }' | jq

# Auction stats with average latency
curl -s http://localhost:8086/v1/reporting/query \
  -H 'Content-Type: application/json' \
  -d '{"table":"auctions","metrics":["count","avg_duration_ms"]}' | jq
```

### Pushing Events Directly (Standalone Mode)

Without NATS, you can push events over HTTP:

```bash
curl -X POST http://localhost:8086/v1/reporting/events \
  -H 'Content-Type: application/json' \
  -d '[{
    "type": "impression",
    "impression": {
      "trace_id": "test-1",
      "campaign_id": "camp-1",
      "creative_id": "cr-1",
      "placement_id": "pl-1",
      "publisher_id": "pub-1",
      "account_id": "acc-1",
      "geo": "GBR",
      "device": "mobile",
      "clearing_price": 2.50,
      "clearing_currency": "USD",
      "clearing_price_usd": 2.50,
      "timestamp": "2024-06-15T10:00:00Z"
    }
  }]'
```

## Using Packages in Go Code

### Report Builder

```go
import "github.com/MichaelJohnWatters/ad-tech-mono/pkg/reporting"
import "github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"

store := analytics.NewMemory()
// ... insert events ...

// Custom query with fluent builder
result, err := reporting.NewBuilder(store).
    Table("impressions").
    Metrics("count", "sum_cost").
    GroupBy("campaign_id", "day").
    ForAccount("acc-123").
    TimeRange(from, to).
    OrderByDesc("sum_cost").
    Limit(10).
    Build(ctx)

// Pre-built report templates
result, _ := reporting.CampaignPerformance.Execute(ctx, store, "acc-123", from, to)
result, _ := reporting.GeoBreakdown.Execute(ctx, store, "acc-123", from, to)
result, _ := reporting.CreativePerformance.Execute(ctx, store, "acc-123", from, to)
result, _ := reporting.PublisherYield.Execute(ctx, store, "pub-456", from, to)
```

### HLL Reach Estimation

```go
import "github.com/MichaelJohnWatters/ad-tech-mono/pkg/reporting"

// Create a sketch per campaign
hll := reporting.DefaultHLL() // precision 14, ~16KB, ~2% error

// Add user IDs as impressions arrive
hll.AddString("user-abc")
hll.AddString("user-def")
hll.AddString("user-abc") // duplicate - won't increase count
fmt.Println(hll.Count())  // ~2

// Merge sketches across time periods (lossless)
daily := hourly1.Clone()
daily.Merge(hourly2)

// Audience overlap between campaigns
overlap := reporting.IntersectionEstimate(campaignA, campaignB)

// Frequency distribution
fd := reporting.NewFrequencyDistribution(map[string]int{
    "user-1": 3, "user-2": 1, "user-3": 5,
})
fmt.Println(fd.AverageFrequency()) // 3.0
fmt.Println(fd.Percentages())      // sorted buckets with percentages

// Forecast reach for a campaign plan
forecast := reporting.ForecastReach(
    50000,   // $50K budget
    2.50,    // $2.50 avg CPM
    200000,  // 200K historical reach
    500000,  // 500K historical impressions
)
// forecast.EstimatedReach, .EstimatedFrequency, .Confidence
```

### Data Pipeline

```go
import "github.com/MichaelJohnWatters/ad-tech-mono/pkg/pipeline"

// Ingest a publisher CSV
file, _ := os.Open("publisher_data.csv")
records, _ := pipeline.IngestCSV(file, ',')

// Also supports TSV
records, _ := pipeline.IngestCSV(tsvFile, '\t')

// Process through validation + normalisation + enrichment
p := pipeline.New(logger)
result := p.Process(ctx, records, pipeline.PublisherConfig{
    PublisherID:    "acme_media",
    RequiredFields: []string{"campaign_id", "impressions"},
    FieldMappings:  map[string]string{
        "campaign": "campaign_id",  // publisher calls it "campaign"
        "imps":     "impressions",  // publisher calls it "imps"
    },
})

fmt.Println(result.Stats.Valid)       // records that passed
fmt.Println(result.Stats.Quarantined) // records that failed
for _, qr := range result.Quarantine {
    fmt.Println(qr.Errors) // why each record failed
}
```

### Datalake (Delta Log)

```go
import "github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/datalake"

dl := datalake.NewMemory(logger)

// Write records with schema tracking
dl.Write(ctx, "normalised/impressions", records, schema)

// Read with filters
records, _ := dl.Read(ctx, "normalised/impressions", datalake.Filter{
    TimeFrom: from,
    TimeTo:   to,
    Columns:  map[string]interface{}{"geo": "GBR"},
})

// Transaction log (Delta Log)
txns, _ := dl.Log(ctx, "normalised/impressions")
// Each txn: version, timestamp, action (add/remove), path, row count

// Current table state
snap, _ := dl.Snapshot(ctx, "normalised/impressions")
// snap.Version, .TotalRows, .ActiveFiles, .Schema
```

### Rollup Engine

```go
import "github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/rollup"

engine := rollup.NewEngine(analyticsStore, clock, logger)
engine.Register(rollup.EventsConfig)   // impressions rollup
engine.Register(rollup.AuctionsConfig) // auctions rollup

// Run hourly rollup (aggregates last completed hour)
results, err := engine.RunLevel(ctx, rollup.Hourly)

// Auto-select the right tier for a query time range
tier := rollup.TierForRange(from, to)
// <= 30min -> Minute, <= 24h -> Hourly, <= 90d -> Daily, else -> Monthly
```

## Running Tests

```bash
# All packages
go test ./pkg/...

# Specific package
go test ./pkg/reporting/ -v

# Specific test
go test ./pkg/reporting/ -run TestHLL_BasicCardinality -v

# With race detector
go test ./pkg/... -race

# Build all service binaries
for svc in gateway exchange dsp tracker ssp adserver reporting pipeline; do
  go build -o ./bin/$svc ./cmd/$svc
done
```

## API Reference

### Exchange (:8081)

| Method | Path | Description |
|---|---|---|
| POST | `/v1/openrtb/auction` | Submit bid request, run auction |
| GET | `/v1/openrtb/win?price=&bid_id=` | Win notice to DSP |
| GET | `/v1/openrtb/loss?bid_id=&reason=` | Loss notice to DSP |

### DSP (:8082)

| Method | Path | Description |
|---|---|---|
| POST | `/v1/openrtb/bid` | Evaluate bid request, return bid |

### Tracker (:8083)

| Method | Path | Description |
|---|---|---|
| GET | `/v1/t/imp?tid=&cid=&pid=&sig=` | Impression pixel (1x1 GIF) |
| GET | `/v1/t/click?tid=&redir=&sig=` | Click redirect (302) |
| GET | `/v1/t/conv?tid=&type=&sig=` | Conversion pixel (1x1 GIF) |
| GET | `/v1/t/view?tid=&dur=&pct=` | Viewability beacon (204) |
| GET | `/v1/t/video?tid=&event=` | Video event (204) |
| GET | `/v1/t/audio?tid=&event=` | Audio event (204) |

### Reporting (:8086)

| Method | Path | Description |
|---|---|---|
| POST | `/v1/reporting/query` | Query analytics store |
| POST | `/v1/reporting/events` | HTTP event ingestion (standalone) |

## Project Structure

```
ad-tech-mono/
  cmd/                    # Service entrypoints (one binary per service)
    gateway/              # API gateway + dashboard + dev tools
    exchange/             # Ad exchange - auctions
    dsp/                  # Demand-side platform - bidding
    tracker/              # Event tracking - pixels
    ssp/                  # Supply-side platform - inventory
    adserver/             # Creative serving
    reporting/            # Analytics + event ingestion
    pipeline/             # Data pipeline
    simulator/            # Traffic simulation CLI
    seed/                 # Seed data loader
    migrate/              # Database migrations
  pkg/                    # Shared packages (all reusable libraries)
    auction/              # Auction engine (5 strategies)
    targeting/            # 11-dimension targeting + bid modifiers
    pacing/               # Budget pacing (even/ASAP/front-loaded)
    reporting/            # Report builder + HLL reach + forecasting
    pipeline/             # Ingest, validate, normalise, enrich
    store/analytics/      # Analytics store (Memory + DuckDB)
    store/rollup/         # Universal rollup framework
    store/datalake/       # Parquet/Delta Log abstraction
    store/postgres/       # Multi-tenant Postgres with RLS
    events/               # EventBus interface (NATS JetStream)
    cache/                # L1 in-process + L2 Redis
    models/               # Domain types
    openrtb/              # OpenRTB 2.6 types
    config/               # Three-layer configuration
    auth/                 # RBAC (5 account types, 6 roles)
    currency/             # Multi-currency conversion
    clock/                # Time abstraction (Real + Fake)
    logger/               # Structured JSON logging + trace IDs
    health/               # /healthz and /readyz
    lifecycle/            # Graceful shutdown
  web/                    # HTML templates + static assets
  k8s/                    # Kubernetes manifests (Kustomize overlays)
    base/                 # Base manifests (all services + infra)
    overlays/local/       # Local dev patches
    overlays/local-lite/  # Lite mode (no observability)
  build/                  # Dockerfiles
  migrations/             # Goose SQL migrations
  profiles/               # Seed data + simulation configs
  docs/                   # PLAN.md (architecture) + openapi.yaml
  tests/                  # E2E smoke tests
```

## Tech Stack

- **Language:** Go everywhere. Python only for ML/data science.
- **Frontend:** Go templates + HTMX + Tailwind CSS. No JS build pipeline.
- **Protocols:** OpenRTB JSON/HTTP (bidding), gRPC (internal), NATS JetStream (async events)
- **Databases:** PostgreSQL (transactional), DuckDB/ClickHouse (analytics)
- **Object storage:** S3 everywhere - Minio locally, real S3 in staging/prod
- **Caching:** L1 in-process + L2 Redis + L3 Postgres
- **Infrastructure:** K8s everywhere, Kustomize overlays, Tilt for dev

## What's Next

Phase 4: Billing and Finance (Steps 36-44) - AuctionWinEvent consumer, double-entry ledger, view-through attribution, variable margin, reconciliation, invoicing.

See `docs/PLAN.md` for the full 123-step build plan across 12 phases.
