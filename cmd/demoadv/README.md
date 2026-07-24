# demoadv — an external demo advertiser ("Ford")

A standalone advertiser website that runs **outside** the cluster and embeds the
platform's real advertiser pixels — the third external origin, alongside
`cmd/demosite` (publisher) and `cmd/extbidder` (DSP). A real advertiser is never
inside your ad-tech cluster; it talks to the platform only via the public tracker
URL (cross-origin).

- **Retargeting** (`/v1/t/rt`) fires on each page view → the visitor is added to
  the advertiser's retargeting audience (consent-gated). Higher-intent pages use
  distinct tags (home vs `f150-interest`).
- **Conversion** (`/v1/t/conv`) fires on "Complete purchase" → a billed conversion.
- A first-party visitor id (a fake hashed email in localStorage) identifies "the
  person", exactly as an advertiser passes its logged-in user's hashed email.
- A consent banner gates retargeting; a bottom-right panel shows the pixels firing.

## Run it

```
make demoadv            # or: go run ./cmd/demoadv   (listens :9200)
open http://localhost:9200
```

Defaults hit the tracker's localhost port (zero setup). For the public path use
`DEMOADV_TRACKER_URL=https://tracker.<domain>` (needs /etc/hosts + the stack on
HTTPS). Requires the stack up + seeded.

## Seeing the audience actually build (the payoff)

The pixel writes a real `behaviour_signals` row (`kind=site_visit`, `user_id`,
`tag`, `account_id`). To turn visits into a targetable audience:

1. Set `DEMOADV_ACCOUNT_ID` to a **real seeded advertiser account id** (so the
   `aid` scopes to a real tenant), and pick a `tag`.
2. Create a **retargeting segment + rule** for that tag (event=`site_visit`,
   tag=`f150-interest`, window=30d) via the audiences API / portal.
3. Run the **profile-builder** (the batch-conductor step, or manually) — it
   matches the rule and enrols the visitor (cluster-expanded across their
   linked ids, household excluded by design).
4. The visitor is now in the segment → a campaign targeting that segment
   retargets them in the next auction.

Verify the raw signal landed:
```sql
SELECT kind, user_id, tag, account_id FROM adtech.behaviour_signals
WHERE tag = 'f150-interest' ORDER BY observed_at DESC LIMIT 5;
```

## Conversion pixel + security (why it may 403)

`/v1/t/conv` is the CPA **billing** trigger, so it validates an HMAC signature
(`tracker.signature_validation`). An unsigned conversion pixel is rejected with
403 — by design (anyone could otherwise fire arbitrary revenue). The realistic
flow: the ad click redirects to the advertiser's landing page carrying a **signed
trace id**, and the conversion pixel reuses it. For a local demo you can set
`tracker.signature_validation=false` to see unsigned conversions fire.

## Separate cluster / deploy

`cmd/demoadv/deploy/demoadv.yaml` — a self-contained Deployment+Service+Ingress
for a second cluster; set `DEMOADV_*` to your platform's public URLs.

## Config (env)

| Var | Default | Meaning |
|---|---|---|
| `DEMOADV_PORT` | `9200` | listen port |
| `DEMOADV_TRACKER_URL` | `http://localhost:8083` | public tracker base (`/v1/t/*`) |
| `DEMOADV_ACCOUNT_ID` | `demo-advertiser` | advertiser account (`aid` — use a real seeded id) |
| `DEMOADV_BRAND` | `Ford` | brand text |
