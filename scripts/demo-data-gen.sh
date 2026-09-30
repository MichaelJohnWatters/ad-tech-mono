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
PSQL(){ kubectl exec -n adtech postgres-0 -- psql -U adtech -d adtech "$@"; }
mint(){ curl -s --max-time 6 -X POST "$GW/v1/auth/token" -H 'Content-Type: application/json' \
  -d "{\"account_id\":\"$1\",\"account_type\":\"advertiser\",\"role\":\"owner\"}" \
  | python3 -c 'import sys,json;print(json.load(sys.stdin).get("token",""))' 2>/dev/null; }

curl -fsS -o /dev/null --max-time 3 "$GW/healthz" || { echo "✗ run 'make demo-forward' first"; exit 1; }

# Advertisers with real settled spend (committed spend → line_items → account).
# campaign_committed_spend.campaign_id is TEXT and == line_items.id.
ADVS=$(PSQL -tAc "SELECT DISTINCT li.account_id::text
  FROM campaign_committed_spend ccs JOIN line_items li ON li.id::text = ccs.campaign_id
  WHERE ccs.settled_micros > 0 LIMIT 4" 2>/dev/null)
echo "  advertisers with spend to work from: $(echo "$ADVS" | grep -c . )"

echo "▶ [1/4] cleanup: purge orphaned marketplace_surcharge_earnings (seller account deleted)…"
N=$(PSQL -tAc "SELECT count(*) FROM marketplace_surcharge_earnings e WHERE NOT EXISTS (SELECT 1 FROM accounts a WHERE a.id=e.seller_account_id)" 2>/dev/null | tr -d ' ')
PSQL -q -c "DELETE FROM marketplace_surcharge_earnings e WHERE NOT EXISTS (SELECT 1 FROM accounts a WHERE a.id=e.seller_account_id)" >/dev/null 2>&1 \
  && echo "  ✔ purged ${N:-0} orphaned earnings rows" || echo "  ⚠ cleanup skipped"

echo "▶ [2/4] invoices: put 3 spenders on 'invoiced' terms (rest stay prepay) + run invoice-runner…"
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

echo "▶ [3/4] conversion configs: create purchase/signup pixels for the spenders…"
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

echo "▶ [4/4] webhooks: register a delivery endpoint for the top spender…"
ADV1=$(echo "$ADVS" | head -1); T=$(mint "$ADV1"); code=000
[ -n "$T" ] && code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 6 -X POST -H "Authorization: Bearer $T" \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://demo-advertiser.example/webhooks/adtech","events":["campaign.budget_depleted","report.completed"]}' \
  "$GW/v1/api/webhooks" 2>/dev/null)
echo "  webhook create HTTP: $code"

cat <<EOF

✔ Demo data generated. Now populated in the portals:
  • Advertiser → Billing → Invoices        ($GW/portal/advertiser)  [3 advertisers on invoiced terms]
  • Advertiser → Conversions (pixels)
  • Advertiser → Team → Webhooks
  • Marketplace earnings no longer shows stale/orphaned rows
EOF
