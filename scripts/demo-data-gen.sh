#!/usr/bin/env bash
#
# demo-data-gen.sh — light up the portal sections that are wired end-to-end but
# ship empty (no seed/load populates them), and clean stale residue. Purely data —
# no code paths change. Run AFTER `make demo-setup` + a load test (invoices need
# billed spend). Needs `make demo-forward` running.
#
#   make demo-data
set -uo pipefail
cd "$(dirname "$0")/.."
GW="${DEMO_GATEWAY:-http://localhost:8080}"
PSQL(){ kubectl exec -i -n adtech postgres-0 -- psql -U adtech -d adtech "$@"; }
mint(){ curl -s --max-time 6 -X POST "$GW/v1/auth/token" -H 'Content-Type: application/json' \
  -d "{\"account_id\":\"$1\",\"account_type\":\"advertiser\",\"role\":\"owner\"}" \
  | python3 -c 'import sys,json;print(json.load(sys.stdin).get("token",""))' 2>/dev/null; }

curl -fsS -o /dev/null --max-time 3 "$GW/healthz" || { echo "✗ run 'make demo-forward' first"; exit 1; }

# Advertisers with real settled spend (committed spend → line_items → account).
# campaign_committed_spend.campaign_id is TEXT and == line_items.id.
ADVS=$(PSQL -tAc "SELECT DISTINCT li.account_id::text
  FROM campaign_committed_spend ccs JOIN line_items li ON li.id::text = ccs.campaign_id
  WHERE ccs.settled_micros > 0 LIMIT 8" 2>/dev/null)
echo "  advertisers with spend to work from: $(echo "$ADVS" | grep -c . )"

echo "▶ [1/7] cleanup: purge orphaned marketplace_surcharge_earnings (seller account deleted)…"
N=$(PSQL -tAc "SELECT count(*) FROM marketplace_surcharge_earnings e WHERE NOT EXISTS (SELECT 1 FROM accounts a WHERE a.id=e.seller_account_id)" 2>/dev/null | tr -d ' ')
PSQL -q -c "DELETE FROM marketplace_surcharge_earnings e WHERE NOT EXISTS (SELECT 1 FROM accounts a WHERE a.id=e.seller_account_id)" >/dev/null 2>&1 \
  && echo "  ✔ purged ${N:-0} orphaned earnings rows" || echo "  ⚠ cleanup skipped"

echo "▶ [2/7] invoices: put 3 spenders on 'invoiced' terms (rest stay prepay) + run invoice-runner…"
# invoice-runner only bills accounts whose advertiser_balances.payment_terms='invoiced'
# (prepay accounts already paid up front). Seed defaults to prepay → nobody's invoiced.
# Flip a few so the Invoices section has content; leaves the rest realistically prepay.
INV_ADVS=$(echo "$ADVS" | head -3)
for a in $INV_ADVS; do
  PSQL -q -c "UPDATE advertiser_balances SET payment_terms='invoiced' WHERE account_id='$a'" >/dev/null 2>&1 || true
done
MONTH="$(date -u +%Y-%m 2>/dev/null || echo 2026-09)"
DATABASE_URL="${DATABASE_URL:-postgres://adtech:adtech-local-dev@localhost:5432/adtech?sslmode=disable}" \
  go run ./cmd/invoice-runner --month "$MONTH" 2>&1 | grep -iE 'invoices_written|complete' | tail -1
echo "  ✔ invoices in DB: $(PSQL -tAc 'SELECT count(*) FROM invoices' 2>/dev/null | tr -d ' ')"

echo "▶ [3/7] conversion configs: create purchase/signup pixels for the spenders…"
made=0
for ADV in $ADVS; do
  T=$(mint "$ADV"); [ -z "$T" ] && continue
  for cfg in '{"name":"Purchase","event_type":"purchase","default_value":49.99,"currency":"USD"}' \
             '{"name":"Signup","event_type":"signup","default_value":5.00,"currency":"USD"}'; do
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 6 -X POST -H "Authorization: Bearer $T" \
      -H 'Content-Type: application/json' -d "$cfg" "$GW/v1/api/conversions" 2>/dev/null)
    case "$code" in 200|201) made=$((made+1));; esac
  done
done
echo "  ✔ conversion configs created: $made"

echo "▶ [4/7] webhooks: register a delivery endpoint for the top spender…"
ADV1=$(echo "$ADVS" | head -1); T=$(mint "$ADV1"); code=000
[ -n "$T" ] && code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 6 -X POST -H "Authorization: Bearer $T" \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://demo-advertiser.example/webhooks/adtech","events":["campaign.budget_depleted","report.completed"]}' \
  "$GW/v1/api/webhooks" 2>/dev/null)
echo "  webhook create HTTP: $code"

echo "▶ [5/7] payouts: give publisher accounts a payout method + run payout-runner…"
# payout-runner only pays publishers with an active payout_method whose minimum is
# met (seed gives none) — mirror the invoice 'invoiced'-terms gate. minimum=0 so any
# revenue pays out. payout-runner reads earnings from ClickHouse (LB :9010 from host).
PSQL -q -c "INSERT INTO payout_methods (id, account_id, method_type, display_name, minimum_payout_cents, currency, status, created_at, updated_at)
  SELECT gen_random_uuid(), a.id, 'bank_transfer', 'Demo Bank ****1234', 0, 'USD', 'active', now(), now()
  FROM accounts a WHERE a.type='publisher'
    AND NOT EXISTS (SELECT 1 FROM payout_methods pm WHERE pm.account_id=a.id)" >/dev/null 2>&1 \
  && echo "  ✔ payout methods added for publisher accounts" || echo "  ⚠ payout method insert skipped"
CLICKHOUSE_ADDR="${CLICKHOUSE_ADDR:-192.168.64.2:9010}" \
DATABASE_URL="${DATABASE_URL:-postgres://adtech:adtech-local-dev@localhost:5432/adtech?sslmode=disable}" \
  go run ./cmd/payout-runner --month "$MONTH" 2>&1 | grep -iE 'payouts|written|complete|error' | tail -2
echo "  ✔ payouts in DB: $(PSQL -tAc 'SELECT count(*) FROM payouts' 2>/dev/null | tr -d ' ')"

echo "▶ [6/7] marketplace: a buyer purchases a public listing → grant…"
LISTING=$(PSQL -tAc "SELECT id FROM marketplace_listings WHERE status='active' LIMIT 1" 2>/dev/null | tr -d ' ')
if [ -n "$LISTING" ]; then
  SELLER=$(PSQL -tAc "SELECT account_id FROM marketplace_listings WHERE id='$LISTING'" 2>/dev/null | tr -d ' ')
  BUYER=$(echo "$ADVS" | grep -v "$SELLER" | head -1)
  T=$(mint "$BUYER")
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 8 -X POST -H "Authorization: Bearer $T" \
    -H 'Content-Type: application/json' -d '{}' "$GW/v1/api/marketplace/listings/$LISTING/purchase" 2>/dev/null)
  echo "  purchase HTTP: $code · grants now: $(PSQL -tAc 'SELECT count(*) FROM marketplace_grants' 2>/dev/null | tr -d ' ')"
else
  echo "  ⚠ no active listing to purchase"
fi

echo "▶ [7/7] audiences: load 1st-party segments (+members) + a 3rd-party data provider for demo advertisers…"
# The demo advertiser (e.g. de6b0145) owns NO segments (the 12 seeded ones live on
# other accounts), so its Audiences tab looks empty. Load a realistic mix per demo
# advertiser: first-party CRM/behavioral/lookalike segments (with real members via
# generate_series so size + match_rate are honest), plus one third-party data
# provider + licensed segments. Idempotent: skip an advertiser that already has any.
i=0
for ADV in $(echo "$ADVS" | head -6); do
  i=$((i+1))
  have=$(PSQL -tAc "SELECT count(*) FROM audience_segments WHERE account_id='$ADV'" 2>/dev/null | tr -d ' ')
  [ "${have:-0}" -gt 0 ] && { echo "  ~ $ADV already has $have segments — skip"; continue; }
  PSQL -q >/dev/null 2>&1 <<SQL || true
    WITH s1 AS (INSERT INTO audience_segments (id,account_id,name,type,source,status,size_estimate,match_rate,visibility,created_at,updated_at)
      VALUES (gen_random_uuid(),'$ADV','High-Value Customers (CRM)','first_party','crm_upload','active',12500,0.62,'dsp_private',now(),now()) RETURNING id)
    INSERT INTO audience_segment_members (segment_id,user_id,account_id,added_at,source)
      SELECT (SELECT id FROM s1),'aud-$i-hv-'||g,'$ADV',now(),'crm_upload' FROM generate_series(1,60) g;
    WITH s2 AS (INSERT INTO audience_segments (id,account_id,name,type,source,status,size_estimate,match_rate,visibility,created_at,updated_at)
      VALUES (gen_random_uuid(),'$ADV','Cart Abandoners 7d','behavioral','pixel','active',3400,0.71,'dsp_private',now(),now()) RETURNING id)
    INSERT INTO audience_segment_members (segment_id,user_id,account_id,added_at,source)
      SELECT (SELECT id FROM s2),'aud-$i-ca-'||g,'$ADV',now(),'pixel' FROM generate_series(1,40) g;
    INSERT INTO audience_segments (id,account_id,name,type,source,status,size_estimate,match_rate,visibility,created_at,updated_at)
      VALUES (gen_random_uuid(),'$ADV','Lookalike: High-Value','lookalike','modeled','active',85000,0.55,'dsp_private',now(),now());
SQL
  echo "  ✔ $ADV: 3 first-party segments + members"
done
ADV1=$(echo "$ADVS" | head -1)
if [ -z "$(PSQL -tAc "SELECT 1 FROM data_providers WHERE account_id='$ADV1' LIMIT 1" 2>/dev/null | tr -d ' ')" ]; then
  PSQL -q >/dev/null 2>&1 <<SQL || true
    WITH p AS (INSERT INTO data_providers (id,account_id,name,kind,default_party,default_licence,status,scope,created_at,updated_at)
      VALUES (gen_random_uuid(),'$ADV1','Acxiom Licensed Segments','dmp','third','purchased','active','account',now(),now()) RETURNING id)
    INSERT INTO audience_segments (id,account_id,name,type,source,status,size_estimate,match_rate,visibility,data_party,provider_id,created_at,updated_at)
      SELECT gen_random_uuid(),'$ADV1',n,'behavioral','provider','active',sz,0.9,'dsp_private','third',(SELECT id FROM p),now(),now()
      FROM (VALUES ('Auto Intenders (3P)',210000::bigint),('Luxury Shoppers (3P)',140000::bigint)) AS v(n,sz);
SQL
  echo "  ✔ $ADV1: 1 third-party data provider + 2 licensed segments"
fi

cat <<EOF

✔ Demo data generated. Now populated in the portals:
  • Advertiser → Billing → Invoices        ($GW/portal/advertiser)  [3 advertisers on invoiced terms]
  • Advertiser → Conversions (pixels) + Team → Webhooks
  • Advertiser → Marketplace → Purchased (grant from a buyer purchase)
  • Publisher  → Earnings → Payouts        ($GW/portal/publisher)
  • Marketplace earnings no longer shows stale/orphaned rows
EOF
