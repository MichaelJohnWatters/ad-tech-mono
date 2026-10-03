# Demo URLs — browsing the local stack

All hostnames below resolve to the Traefik load balancer at **`192.168.64.2`** via
this Mac's `/etc/hosts` (run `make hosts` once — `scripts/hosts-setup.sh` writes the
managed block). No port-forward needed to *browse*; `localhost` ports are only for
host tools / e2e that hardcode them (see the bottom table).

> **TLS:** every `https://*.adtech.local` uses a self-signed cert — your browser shows
> a one-time warning you click through. These entries are local to this machine only;
> they are **not** shareable with anyone else (that needs the staging deploy — see
> `docs/DEPLOY.md`).

Source of truth for this list: `kubectl get ingress -A`.

## Portal (customer + staff dashboard)

| URL | What |
|---|---|
| https://gateway.adtech.local | Gateway — portal, REST API (`/v1/api/*`), Swagger at `/docs` |
| https://adtech.local | Same gateway (shorter alias) — **needs `make hosts` re-run** (older runs only added `*.adtech.local` subdomains, not the bare host) |

The portal is one gateway; which sections you see (advertiser / publisher / staff /
ops) depends on the account you log in as. Dev logins are planted by `make seed`
(e.g. staff-ops `devops@adtech.local`). The staff **Architecture** page (C4 diagrams)
and **Ops** section live here.

## Demo publisher sites (the showcase — each its own brand)

| URL | Mimics | Ad format shown |
|---|---|---|
| https://viewtube.adtech.local | YouTube | VMAP video — watch pages at `/watch/1` (pre), `/watch/2` (pre+mid), `/watch/3` (pre+mid+post) |
| https://twitchr.adtech.local | Twitch | Continuous **live SSAI** channel (`/` = the live page) |
| https://soundwave.adtech.local | Spotify | DAAST audio |
| https://primereel.adtech.local | Netflix | Pre-roll video on watch |
| https://chronicle.adtech.local | A newspaper | Display + native in articles |
| https://gadget.adtech.local | A tech blog | Display + native in a review feed |

Every site has a collapsible **"behind the scenes" trace panel** showing the live
ad calls, auction outcome (fill / no-bid / house), and SSP-resolved audience segments.

## Demo advertiser (the conversion side)

| URL | What |
|---|---|
| https://shop.adtech.local | Demo advertiser shop — fires the retargeting pixel + signed `/v1/t/conv` postback (the CPA money loop) |

## Platform / internal services (direct, for debugging)

| URL | Service |
|---|---|
| https://ssp.adtech.local | SSP |
| https://exchange.adtech.local | Exchange |
| https://dsp.adtech.local · https://dsp-int.adtech.local | Internal DSP |
| https://dsp-comp1..4.adtech.local | Competitor DSP simulators |
| https://adserver.adtech.local | Ad server |
| https://pubad.adtech.local | Publisher ad server (`/v1/pubad/*`, incl. VMAP) |
| https://tracker.adtech.local | Tracker (beacons) |

## localhost fallbacks (host tools / e2e only)

`make demo-forward` (`scripts/demo-forward.sh`) port-forwards these for tools that
hardcode `localhost`. Run it in its own terminal; Ctrl-C tears the tunnels down.

| Port | Service | | Port | Service |
|---|---|---|---|---|
| 8080 | gateway | | 8089 / 8090 | dsp-competitor1 / 2 |
| 8081 | exchange | | 8093 | ssai |
| 8082 | dsp-internal | | 8094 | transcoder |
| 8083 | tracker | | 5432 | postgres |
| 8084 | ssp | | 6379 | redis |
| 8085 | adserver | | 9000 | minio |
| 8086 | reporting | | 3000 | grafana |
| 8088 | publisher-adserver | | 16686 | jaeger |

Not part of `demo-forward`: `make devconsole` runs the host dev-loop UI at
`localhost:8099` separately.
