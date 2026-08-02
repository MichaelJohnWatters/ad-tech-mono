#!/usr/bin/env bash
# security-harness.sh — turn the local stack's anti-spoofing mechanisms to
# ENFORCE (strict) as a security test harness, or revert them.
#
# Enforces all four externally-facing anti-spoof controls and sets up the
# prerequisites so LEGITIMATE traffic still passes:
#   1. ads.cert  — exchange Ed25519-signs bid requests; DSPs verify (strict).
#   2. ads.txt   — publishers must authorise our exchange (strict).
#   3. schain    — SSP originates a SupplyChain node; exchange validates (strict).
#   4. tracker   — pixel HMAC signature validation (already on by seed).
#
# It is IDEMPOTENT and does NOT change the committed chart defaults (so normal
# dev / e2e are unaffected unless you run this). Verify with the spoof tests
# printed at the end.
#
#   scripts/security-harness.sh on     # set up prereqs + enforce strict
#   scripts/security-harness.sh off    # revert enforcement to the dev defaults
#   scripts/security-harness.sh status # show current enforcement config
set -euo pipefail
NS=adtech
# A FIXED dev Ed25519 keypair (local test only — prod uses a real SOPS secret).
DEV_ADCERT_PRIV="2NJwLroe2MI9bzeybevvfM6gW6PJXKyeG74694z0T9dtZw48XO2f03TC-Hx_NzaiGF0QqCb_QLh8WP49pdKHcA"
SELLER_DOMAIN="adtech.local"
EXCHANGE_ID="adtech-exchange"
SSP_ID="adtech-ssp"

psql() { kubectl -n "$NS" exec postgres-0 -- psql -U adtech -d adtech -tAc "$1"; }
# setcfg writes a GLOBAL (pod_id='') live-config row so every pod — the exchange
# that enforces AND the gateway that surfaces the policy to publishers — reads the
# same value. Upsert, not UPDATE: a fresh stack may not have the row yet.
setcfg() {
  psql "INSERT INTO config (key,value,service,pod_id,updated_by)
        VALUES ('$1','\"$2\"','platform','','security-harness')
        ON CONFLICT (pod_id,key) DO UPDATE SET value=EXCLUDED.value, updated_at=now()" >/dev/null
}

case "${1:-status}" in
on)
  echo "▶ 1/4 wiring prerequisites (signing keys, identities, schain origin)…"
  # ads.cert sign key is a fallback only — the exchange prefers the active
  # adcert_ed25519 secret from the store (Phase I). The seller identity is set as
  # a GLOBAL config row below (step 4) so the gateway can surface it to publishers.
  kubectl -n "$NS" set env deploy/exchange \
    EXCHANGE_ADCERT_SIGN_KEY="$DEV_ADCERT_PRIV" >/dev/null
  for d in dsp-internal dsp-competitor1 dsp-competitor2; do
    kubectl -n "$NS" set env deploy/$d DSP_ADCERT_KEY_URL="http://exchange:8081/v1/adcert/key" >/dev/null
  done
  kubectl -n "$NS" set env deploy/ssp SSP_SELLER_DOMAIN="$SELLER_DOMAIN" SSP_SELLER_ID="$SSP_ID" >/dev/null

  echo "▶ 2/4 authorising seeded publishers in ads_txt_cache…"
  # AdsTxtEntry has NO json tags → keys must be the Go FIELD names (Domain/AccountID/Relationship).
  AUTH="[{\"Domain\":\"$SELLER_DOMAIN\",\"AccountID\":\"$EXCHANGE_ID\",\"Relationship\":\"DIRECT\"}]"
  for dom in $(psql "SELECT domain FROM publishers"); do
    psql "INSERT INTO ads_txt_cache (domain, entries, status, last_fetched, last_changed)
          VALUES ('$dom', '$AUTH'::jsonb, 'valid', now(), now())
          ON CONFLICT (domain) DO UPDATE SET entries=EXCLUDED.entries, status='valid', last_fetched=now()" >/dev/null
  done
  # A domain that PUBLISHES an ads.txt EXCLUDING us — the reject-test fixture.
  psql "INSERT INTO ads_txt_cache (domain, entries, status, last_fetched, last_changed)
        VALUES ('spoofer.example', '[{\"Domain\":\"competitor.io\",\"AccountID\":\"x\",\"Relationship\":\"DIRECT\"}]'::jsonb, 'valid', now(), now())
        ON CONFLICT (domain) DO UPDATE SET entries=EXCLUDED.entries" >/dev/null

  echo "▶ 3/4 rolling exchange/ssp/dsps to pick up the keys…"
  kubectl -n "$NS" rollout restart deploy/exchange deploy/ssp deploy/dsp-internal deploy/dsp-competitor1 deploy/dsp-competitor2 >/dev/null
  for d in exchange ssp dsp-internal dsp-competitor1 dsp-competitor2; do
    kubectl -n "$NS" rollout status deploy/$d --timeout=120s >/dev/null
  done

  echo "▶ 4/4 flipping enforcement → strict (live config)…"
  # Platform seller identity — global rows so BOTH the exchange (enforcement) and
  # the gateway (publisher-facing /v1/api/integration/adstxt) resolve the same line.
  setcfg exchange.adstxt_seller_domain "$SELLER_DOMAIN"
  setcfg exchange.adstxt_seller_id "$EXCHANGE_ID"
  setcfg dsp.adcert_enforcement strict
  setcfg exchange.adstxt_enforcement strict
  setcfg exchange.schain_enforcement strict
  setcfg tracker.signature_validation true
  # Per-advertiser conversion keys (G7) are already default-on (values.yaml), but
  # a churned/dev stack may have a stale pod row — assert it here so the harness
  # is self-contained. Seeded keys + DevConversionKey signing keep legit traffic
  # working; a conversion signed with the shared platform key for an advertiser
  # that HAS a key is rejected.
  setcfg tracker.conversion_strict_advertiser_key true
  echo "✓ enforced. (config poll ≤30s.) Verify:"
  cat <<'EOF'
  # legit fills:   go run ./cmd/simulator run --profile steady --requests 100 --rps 25
  # adstxt reject: curl -s -XPOST localhost:8081/v1/openrtb/auction -d '{"id":"s","site":{"domain":"spoofer.example"},"source":{"ext":{"schain":{"ver":"1.0","nodes":[{"asi":"adtech.local","sid":"adtech-ssp","hp":1}]}}},"imp":[{"id":"1","banner":{"w":300,"h":250},"bidfloor":1}],"cur":["USD"]}'  # → nobid
  # schain reject: same as above but omit "source"  # → nobid
  # adcert reject: curl -s -XPOST localhost:8082/v1/openrtb/bid   -d '{"id":"s","site":{"domain":"daily-news.com"},"imp":[{"id":"1","banner":{"w":300,"h":250},"bidfloor":1}],"cur":["USD"]}'  # unsigned → nobid
  # pixel reject:  curl -s -o/dev/null -w '%{http_code}\n' 'localhost:8083/v1/t/imp?tid=x&cid=y'  # → 403
EOF
  ;;
off)
  echo "▶ reverting enforcement to dev defaults…"
  setcfg dsp.adcert_enforcement off
  setcfg exchange.adstxt_enforcement off
  setcfg exchange.schain_enforcement warn
  # tracker sig validation is left ON (it's the seeded default).
  echo "✓ enforcement off (adcert off, adstxt off, schain warn). Prereq env/keys remain (harmless)."
  ;;
status)
  psql "SELECT key, value FROM config WHERE key IN ('dsp.adcert_enforcement','exchange.adstxt_enforcement','exchange.schain_enforcement','tracker.signature_validation') ORDER BY key"
  ;;
*)
  echo "usage: $0 {on|off|status}"; exit 1 ;;
esac
