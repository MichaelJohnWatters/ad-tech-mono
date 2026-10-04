# Demosite — External Demo Publisher

A standalone publisher website that embeds the **real** `adtech.js` SDK / VAST tags and
requests real ads from the platform's PUBLIC surfaces. Deliberately runs **outside** the
ad-tech cluster (host process on `:9000`, or a second cluster via `deploy/demosite.yaml`)
so it exercises the true cross-origin path — CORS, TLS, configurable SDK host, public
ingress — that an in-cluster page would paper over. Not in the Helm chart on purpose.

## How it runs

- `make demosite` (or `go run ./cmd/demosite`) → http://localhost:9000. Needs the stack up + seeded (`pub-simulator` publisher, `pl-sim-{mpu,video,audio,native}` placements).
- `make demosites` → three distinct branded origins from the same binary via `DEMOSITE_SITE` (chronicle `:9001`, gadget `:9002`, streamhub `:9003`).
- Config is plain env (see README table): `DEMOSITE_PORT`, `DEMOSITE_PUBAD_URL` (default `http://localhost:8088`), `DEMOSITE_SDK_URL`, `DEMOSITE_MEDIA_URL`, `DEMOSITE_PUBLISHER_ID`, `DEMOSITE_{DISPLAY,VIDEO,AUDIO,NATIVE}_PLACEMENT`, `DEMOSITE_SITE`.
- Public-TLS and separate-cluster variants: `README.md` in this directory (the how-to source of truth — don't duplicate it here).

## Pages

- `/` display+native, `/video` VAST pre-roll, `/audio` VAST audio, `/native` in-feed; `/healthz`.
- `/pages` + `/p/{slug}` — multi-slot combo pages defined ONCE in `pkg/simulator/pages`
  (the same layouts the zero-slippage e2e replays; `DEMOSITE_SITE` restricts each branded
  origin to its own layout set via `pages.SiteBySlug`).

## What it talks to (browser-side, public URLs only)

- Publisher ad server `/v1/pubad/*` (`:8088`) — serve/VAST/VMAP/native/audio (`pkg/routes`)
- Gateway `:8080` — `adtech.js` SDK + creative/media assets
- Never in-cluster DNS: "host process now, second cluster later" is a deploy choice, not a code change.

## CRITICAL

- **Stays a third party.** It intentionally does NOT use `pkg/config`/`pkg/routes`/`pkg/logger`
  — plain env + stdlib only, like a real external publisher. Don't "fix" this to match the
  `cmd/` service conventions; wiring it to internal packages/URLs defeats its purpose.
- Page layouts live in `pkg/simulator/pages` — add/change combo pages THERE, not in
  templates here, or the e2e (`tests/e2e/multisite_test.go`, `TestMultiSitePublishersEndToEnd`)
  and the demosite drift apart.
- Templates are `go:embed`ed — the image is self-contained (`SERVICE=demosite`).
- Mixed content: an HTTPS-hosted demosite needs the platform's browser-facing tracker/media
  URLs to be HTTPS too (see README note + `k8s/helm/adtech/values-prod.yaml`).
- A consent banner gates ad loading (personalised = TCF purposes 1-4, else contextual-only);
  keep it — it's the demo of the consent-through-the-chain rule.

## Pointers

- `docs/PLAN.md` → "Publisher SDK Documentation", "Publisher Simulator", "`pub-simulator` is a first-class seeded publisher"
- `k8s/CLAUDE.md` → "External demo origins" (demosite + demoadv + extbidder)
- Multi-slot layout truth: `pkg/simulator/pages`

## Diagram Updates

If you change this component, check if diagrams need updating:
- **New platform surface called (new pubad/gateway endpoint)?** Update `docs/PLAN.md` → the relevant endpoint section
- **New external-origin topology (extra site, second cluster)?** update the C4 model (`docs/diagrams/workspace.dsl`) and run `make c4`
