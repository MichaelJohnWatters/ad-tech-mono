# viewabilitysmoke - Browser Viewability Smoke Driver

Host tool (never deployed). Drives a REAL headless Chrome (chromedp) through the demo
publisher's video page so the page's own IntersectionObserver self-measures IAB video
viewability and fires the signed beacon — the one client-side step the HTTP-only e2e
harness can't exercise (no browser DOM).

## How It Runs

- Opt-in only: `make viewability-smoke` → `scripts/viewability-smoke.sh`; never part
  of `make test-e2e`. Needs the live stack up + seeded, Google Chrome, and the
  localhost LB tunnels (pubad :8088, gateway :8080) — `make stack-doctor` if dead.
- The script asserts; this binary only DRIVES. Script: baseline `adtech.views`
  (channel=video, iab_viewable=1) in ClickHouse → start `cmd/demosite` on :9000 →
  run this binary → poll for the count to rise. Binary: navigate → wait for the SDK
  → `applyConsent(true)` → wait for `<video>` → scroll into view → play the dwell.
- Flags: `-url` (default `http://localhost:9000/p/video-hub`), `-headless`, `-dwell`
  (default 8s; must exceed the observer's 2s dwell). Hard 60s context timeout.

## Gotchas

- Sends a real-Chrome UA on purpose: the default headless UA contains
  "HeadlessChrome", which matches the tracker's bot patterns (`headlesschrome` in
  `pkg/fraud/realtime.go`) and the beacon is silently dropped. Don't "simplify" it away.
- Consent must be applied AFTER the SDK loads (`applyConsent()` calls into it); the
  `<video>` exists only after the VAST fills, hence the WaitReady.
- Changed `web/static/adtech.js`? Rebuild + redeploy the gateway first — its image
  ships `web/static` and serves the SDK at `/static/adtech.js`.

## Pointers

- `docs/PLAN.md` -> "Viewability (adtech.js SDK)"; `cmd/tracker/CLAUDE.md`
  (`/v1/t/view` beacon, fraud checks).
