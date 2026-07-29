# demosite — an external demo publisher

A standalone publisher website that embeds the **real** `adtech.js` SDK / VAST
tags and requests **real** ads from the platform's public ad server. It runs
**outside** the ad-tech cluster on purpose — a real publisher is never inside
your cluster, and running it as a separate origin is what exercises the true
cross-origin path (CORS, TLS, configurable SDK host, public ingress) that an
in-cluster page would hide.

Pages: `/` (display + native), `/video` (VAST pre-roll), `/audio` (VAST audio),
`/native` (in-feed). A consent banner gates ad loading (personalized vs
contextual — TCF purposes 1–4).

## Run it (host process — the default)

```
make demosite            # or: go run ./cmd/demosite
open http://localhost:9000
```

Zero setup: the defaults point at the localhost-exposed platform ports
(`pubad :8088`, SDK + media from the gateway `:8080`), so ads fill immediately
in a browser with no `/etc/hosts` edits and no TLS. Requires the local stack up
and seeded (`make stack-up && make seed`) — it uses the `pub-simulator`
placements (`pl-sim-mpu|video|audio|native`).

## Run it against the PUBLIC path (TLS ingress + CORS)

To validate exactly what a cloud-hosted publisher hits (real HTTPS, real CORS),
point it at the ingress hostnames. Add them to `/etc/hosts` first:

```
127.0.0.1 gateway.adtech.local adserver.adtech.local tracker.adtech.local ssp.adtech.local pubad.adtech.local
```

```
DEMOSITE_PUBAD_URL=https://pubad.adtech.local \
DEMOSITE_SDK_URL=https://gateway.adtech.local/static/adtech.js \
DEMOSITE_MEDIA_URL=https://gateway.adtech.local \
  go run ./cmd/demosite
```

## Run it in a SEPARATE cluster (most faithful)

`deploy/demosite.yaml` is a self-contained Deployment+Service+Ingress for a
second `k3d`/`kind` cluster (or any cluster that is NOT the ad-tech one). Build
the image (`SERVICE=demosite`, templates are `go:embed`ed so it's self-contained)
and set the `DEMOSITE_*` env to your platform's **public** URLs. The demosite
then calls the ad-tech cluster's external IP/hostname — a genuine cross-cluster
programmatic path.

## Config (env)

| Var | Default | Meaning |
|---|---|---|
| `DEMOSITE_PORT` | `9000` | listen port |
| `DEMOSITE_PUBAD_URL` | `http://localhost:8088` | public base of the publisher-adserver (`/v1/pubad/*`) |
| `DEMOSITE_SDK_URL` | `http://localhost:8080/static/adtech.js` | where the browser loads `adtech.js` |
| `DEMOSITE_MEDIA_URL` | `http://localhost:8080` | base for creative/media assets |
| `DEMOSITE_PUBLISHER_ID` | `pub-simulator` | seeded demo publisher |
| `DEMOSITE_{DISPLAY,VIDEO,AUDIO,NATIVE}_PLACEMENT` | `pl-sim-*` | seeded placement external IDs |
| `DEMOSITE_SITE` | _(unset)_ | render a named "friend's website" property (`chronicle`/`gadget`/`streamhub`, from `pkg/simulator/pages`) — its own branding + page set. Unset = the default "Demo Times" showing every layout. |

**Multiple branded sites at once.** `make demosites` runs three distinct
branded origins — chronicle (`:9001`), gadget (`:9002`), streamhub (`:9003`) —
each a separate `DEMOSITE_SITE`. They serve real ads against the local stack; the
true per-tenant isolation + revenue split is proven by
`TestMultiSitePublishersEndToEnd` (multi-publisher, multi-advertiser, zero
slippage).

> Note on mixed content: when the demo page itself is served over HTTPS (e.g.
> deployed to the cloud), the platform's returned tracker/media URLs must also be
> HTTPS or the browser blocks them. Set the serving services' public browser-facing
> URLs (`ADSERVER_TRACKER_URL`, `PUBLISHER_ADSERVER_TRACKER_URL`, media base) to
> your public HTTPS host — these are prod-values concerns already flagged in
> `k8s/helm/adtech/values-prod.yaml`. On the local http://localhost:9000 page the
> default localhost URLs work fine.
