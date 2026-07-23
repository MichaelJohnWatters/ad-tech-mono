# extbidder — an external DSP partner

A standalone OpenRTB bidder that simulates a **real competitor DSP running
outside the cluster**. It speaks only OpenRTB (`POST /v1/openrtb/bid`) and holds
no shared state — no Postgres/Redis/NATS. The in-cluster **exchange** fans out
bid requests to it over the network, so it exercises the real **outbound** path:
cross-network OpenRTB, the auction timeout budget, and a bidder the platform
doesn't control. It's the demand-side mirror of `cmd/demosite`.

## Run it (host process)

```
make extbidder          # or: go run ./cmd/extbidder   (listens :9100)
```

It bids `floor × (1 + markup)` on most requests, no-bids a configurable fraction,
and returns a renderable creative branded "EXTERNAL DSP" (HTML banner / VAST
video / DAAST audio / native JSON).

## Wire the exchange to call it

The exchange's DSP list is the **live config** key `exchange.dsp_endpoints`
(live config overrides the env var). Append the bidder's URL. From inside the
cluster the host is reachable at `host.docker.internal`:

```sql
-- via the config table (pod-scoped; exchange pod is "exchange-0"):
UPDATE config
SET value = '"http://dsp-internal:8082,http://dsp-competitor1:8089,http://dsp-competitor2:8090,http://host.docker.internal:9100"'
WHERE key = 'exchange.dsp_endpoints' AND pod_id = 'exchange-0';
```

(or set it through the config-manager UI). The exchange picks it up on its next
config poll (≤30s); confirm with the router inspector:

```
curl 'localhost:8081/debug/exchange/routing?preview=true&channel=display'
```

Then run traffic (`make traffic` / the simulator) and watch it compete:

```sql
-- ClickHouse: bids + latency per DSP (the external hop shows higher latency)
SELECT dsp_endpoint, count() calls, countIf(bid_received) bids,
       round(avg(latency_ms)) lat_ms
FROM adtech.dsp_calls WHERE timestamp > now() - INTERVAL 5 MINUTE
GROUP BY dsp_endpoint;
-- wins: the external endpoint is the last dsp-N index (e.g. dsp-3)
SELECT winner_dsp, count() FROM adtech.auction_wins
WHERE timestamp > now() - INTERVAL 5 MINUTE GROUP BY winner_dsp;
```

> Remember to remove `host.docker.internal:9100` from `exchange.dsp_endpoints`
> when the bidder isn't running — otherwise every auction wastes its timeout on a
> dead endpoint.

## Separate cluster (most faithful)

Build the image (`SERVICE=extbidder`) and run it in a second cluster / any host
with a public endpoint, then point `exchange.dsp_endpoints` at that public URL —
a genuine cross-cluster programmatic auction to an external partner.

## Config (env)

| Var | Default | Meaning |
|---|---|---|
| `EXTBIDDER_PORT` | `9100` | listen port |
| `EXTBIDDER_SEAT` | `ext-partner-dsp` | OpenRTB seat |
| `EXTBIDDER_BRAND` | `Partner DSP` | creative brand text |
| `EXTBIDDER_MARKUP` | `0.35` | bid = floor × (1 + markup) |
| `EXTBIDDER_NOBID_RATE` | `0.2` | fraction of requests it no-bids |
| `EXTBIDDER_ADOMAIN` | `partner-dsp.example` | advertiser domain |
| `EXTBIDDER_VIDEO_URL` / `EXTBIDDER_AUDIO_URL` | sample URLs | VAST/DAAST media |
