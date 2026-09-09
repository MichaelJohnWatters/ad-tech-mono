# advertisersmoke Tool

Headless-Chrome smoke for the BUY-SIDE tag: drives a real browser through the
demo advertiser site (`cmd/demoadv`), gives consent (the shared `adtech-adv.js`
tag fires the retargeting pixel / site_visit) and calls the page's `convert()`
(demoadv's `/convert` fires the signed S2S conversion). Buy-side twin of
`cmd/viewabilitysmoke`. Host-only, never deployed.

## How it runs

- `make advertiser-smoke` → `scripts/advertiser-smoke.sh`: starts demoadv (`:9200`,
  SDK from the gateway's `/static/adtech-adv.js`), runs this binary, then asserts
  BOTH rows land in ClickHouse (`behaviour_signals kind='site_visit'` +
  `conversions` counts increase) — browser → adtech-adv.js → tracker → NATS →
  reporting → ClickHouse. Needs the live stack up + seeded, and real Chrome.
- Flags: `-url` (default `http://localhost:9200/models/f150`), `-headless`,
  `-type` (default `purchase`), `-rev`. Script overrides: `DEMOADV_TRACKER_URL`,
  `DEMOADV_GATEWAY_URL`, `DEMOADV_PORT`, `ADVERTISER_PAGE`.

## Gotchas

- This binary asserts NOTHING downstream — the ClickHouse assertion lives in the
  script; don't run the binary alone and call it a pass.
- Spoofs a real Chrome UA on purpose: the default headless UA matches the
  tracker's bot patterns (`headlesschrome` in `pkg/fraud/realtime.go`) and the
  pixel is silently dropped.
- The gateway image bakes + serves the SDK (`build/Dockerfile.gateway` copies
  `web/static`) — rebuild/redeploy the gateway after editing
  `web/static/adtech-adv.js` or the smoke exercises the stale tag.

## Pointers

- `docs/attribution-plan.md` (signed conversion postback); `docs/DYNAMIC-PRODUCT-ADS.md`
  (advertiser SDK). Sell-side twin: `make viewability-smoke`.
