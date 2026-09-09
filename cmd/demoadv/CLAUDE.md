# demoadv — External Demo Advertiser Site (host tool)

A standalone advertiser shop ("Ford" by default) that runs OUTSIDE the cluster
and embeds the platform's real advertiser pixels. Third external origin,
alongside `cmd/demosite` (publisher) and `cmd/extbidder` (external DSP). It is
the click-through landing/conversion side of the money loop: ad click → lands
here (the `adtech-adv.js` tag captures `?adtech_tid=` first-party) → browse
fires retargeting → checkout fires a signed S2S conversion → CPA settles.

## How it runs

- `make demoadv` / `go run ./cmd/demoadv` — host process on `:9200`
  (`DEMOADV_PORT`). Never deployed by the Helm chart; for a second cluster use
  the self-contained manifest `cmd/demoadv/deploy/demoadv.yaml`.
- Needs the stack up + seeded. Full env table, audience-build walkthrough, and
  the `behaviour_signals` verification SQL: `cmd/demoadv/README.md`.
- Re-skinnable without forking templates: `DEMOADV_BRAND` / `DEMOADV_PRODUCT` /
  `DEMOADV_SKU` (the themed world runs it as a dog-food shop; default SKU
  `DOG-KIBBLE-12KG` matches the seeded DPA catalog). Routes stay stable.

## Interfaces (HTTP only — no gRPC, no NATS, no DB)

- `GET /`, `/models/f150`, `/checkout` — shop pages. Each embeds the shared
  `adtech-adv.js` tag (loaded from `DEMOADV_SDK_URL`, default gateway
  `/static/adtech-adv.js`), which on consent fires the browser retargeting
  pixel `/v1/t/rt` with a per-page `tag` (`home` / `f150-interest` /
  `checkout`) and the page's SKU (`skus=`, Dynamic Product Ads).
- `POST /convert` — server-to-server SIGNED conversion postback: browser POSTs
  the sale facts here; the handler builds `/v1/t/conv` (uid from the
  `adtechadv_uid` cookie, `ctid` from the `adtech_ctid` click-trace cookie,
  purchased `skus`), signs it with `adserving.SignURL` + the advertiser's key
  (`DEMOADV_SIGNING_KEY`, defaulting to `adserving.DevConversionKey(accountID)`
  so seeded strict per-advertiser validation passes zero-setup), and fires it.
- `GET /healthz` — always ok.

## CRITICAL

- **Conversions are the CPA billing trigger — S2S signed only.** `/v1/t/conv`
  must NEVER be browser-fired or unsigned (billing forgery); keep the
  browser → `/convert` → signed-tracker-URL shape.
- **External boundary means the public tracker URL, cross-origin, nothing
  else.** No Postgres/Redis/NATS, no `pkg/config`/`pkg/logger`/`pkg/lifecycle`
  — the `cmd/` shared-package conventions do NOT apply (only `pkg/adserving`
  for signing). Don't "fix" it by wiring in platform packages: it models a
  real advertiser who is never inside your cluster.
- **`DEMOADV_ACCOUNT_ID` must be a real seeded advertiser id** for retargeting
  rules to match; the `demo-advertiser` placeholder fires pixels but builds no
  audience.
- The persona bar derives a deterministic TEST-NET demo IP from the persona
  email (rides the tracker's allowlist-gated `?ip=`, inert in prod) so this
  shop and `cmd/demosite` share one household per persona — anonymous guest
  carts stay chaseable via household id even without an email capture.

## Pointers

- `docs/PLAN.md` -> "Tracker Endpoints (HTTP - External Facing)"
- `docs/PLAN.md` -> "Identity and First-Party Data" (hashed-email bridging)
- `docs/PLAN.md` -> "View-Through Conversion Attribution"
- `docs/DYNAMIC-PRODUCT-ADS.md` (SKU pixel → dynamic creative → suppression)
- `docs/attribution-plan.md` (`ctid` click-through attribution + G7 keys)
