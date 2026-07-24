# Anti-spoofing / authentication model

How each externally-reachable call path is protected against spoofing, and how to
run the **local enforcement test harness** that proves it.

## The model

A shared secret can't live in a **browser** (devtools exposes it), so browser-fired
calls use *server-issued signed URLs* + *supply-chain auth* + *fraud checks*. Only
**server-to-server** calls carry a real shared secret / signature.

| Call path | Origin | Mechanism | Default |
|---|---|---|---|
| Tracker `imp`/`click`/`conv` pixels | advertiser (browser) | **HMAC-signed URL** (`sig`, server-issued) — `tracker.signature_validation` | ⚠️ off by default, **on in the seeded stack** |
| Exchange → external DSP `/bid` | DSP (S2S) | **ads.cert** — Ed25519-signed bid requests, DSP verifies + anti-replay (`dsp.adcert_enforcement`) | ❌ off |
| Publisher inventory claims | publisher | **ads.txt / sellers.json** supply-chain auth (`exchange.adstxt_enforcement`) | ❌ off |
| SupplyChain object | SSP→exchange | **schain** structural validation (`exchange.schain_enforcement`) | ⚠️ warn |
| `/v1/api/*` management | portal/API | **JWT + `X-API-Key`** | ✅ on |

Signing keys / material:
- **Tracker HMAC**: `tracker.signing_key` (the ad servers sign with the matching
  key; both default `adtech-dev-signing-key-change-in-prod`).
- **ads.cert**: `exchange.adcert_sign_key` (Ed25519 private, base64) → the exchange
  publishes its public key at `GET /v1/adcert/key`; DSPs fetch it via
  `dsp.adcert_key_url` (or a static `dsp.adcert_verify_key`).
- **ads.txt identity**: `exchange.adstxt_seller_domain` + `exchange.adstxt_seller_id`
  must appear (as `Domain`,`AccountID`,`DIRECT`) in each publisher's ads.txt.
- **schain origin**: `ssp.seller_domain` + `ssp.seller_id` (the first schain node).

## Local enforcement test harness

The four controls are off/warn by default (so dev/e2e aren't affected). Turn them
all to **strict** — with the prerequisites set up so legitimate traffic still
passes — and prove both accept-legit and reject-spoofed:

```bash
make security-harness ARG=on        # set up keys/identity/ads.txt + enforce strict
make security-harness ARG=status
make security-harness ARG=off       # revert to dev defaults
```

`on` does: sets the exchange adcert key + ads.txt identity, points DSPs at the
exchange's public key, sets the SSP schain origin, authorises every seeded
publisher in `ads_txt_cache` (+ one `spoofer.example` that excludes us), rolls the
pods, then flips all four to strict.

### Verified behaviour (both directions)

| Mechanism | Accept legit | Reject spoofed |
|---|---|---|
| ads.cert | DSPs bid on exchange-signed requests | unsigned request → `nobid` (identical request *bids* with adcert off — proves it's the signature) |
| ads.txt | `daily-news.com` (authorised) → real bid | `spoofer.example` (excludes us) → `nobid` |
| schain | valid schain → bid | missing schain (same domain) → `nobid` |
| tracker HMAC | signed pixel → recorded | unsigned `/v1/t/imp` → **403** |

Legit traffic still fills ~77% with all four strict — enforcement doesn't break
real flows.

### Prod
Set these strict in `values-prod.yaml` (see `docs/DEPLOY.md`), supply a real
`exchange.adcert_sign_key` + `tracker.signing_key` via SOPS, and let the daily
`cmd/adstxt` CronJob populate `ads_txt_cache` from publishers' real ads.txt files.

## Known open gaps (see the "gaps" discussion)
- **Retargeting pixel `/v1/t/rt`** is unsigned — `aid` is claimed, not proven
  (audience-only, consent-gated). Fix: unguessable per-advertiser pixel token.
- **`/v1/pubad/serve`** has no per-request publisher auth (relies on ads.txt/schain).
- **Prebid S2S endpoint** is open (`partner_shared` secret purpose defined, unused).
