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

echo "▶ [1/8] cleanup: purge orphaned marketplace_surcharge_earnings (seller account deleted)…"
N=$(PSQL -tAc "SELECT count(*) FROM marketplace_surcharge_earnings e WHERE NOT EXISTS (SELECT 1 FROM accounts a WHERE a.id=e.seller_account_id)" 2>/dev/null | tr -d ' ')
PSQL -q -c "DELETE FROM marketplace_surcharge_earnings e WHERE NOT EXISTS (SELECT 1 FROM accounts a WHERE a.id=e.seller_account_id)" >/dev/null 2>&1 \
  && echo "  ✔ purged ${N:-0} orphaned earnings rows" || echo "  ⚠ cleanup skipped"

echo "▶ [2/8] invoices: put 3 spenders on 'invoiced' terms (rest stay prepay) + run invoice-runner…"
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

echo "▶ [3/8] conversion configs: create purchase/signup pixels for the spenders…"
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

echo "▶ [4/8] webhooks: register a delivery endpoint for the top spender…"
ADV1=$(echo "$ADVS" | head -1); T=$(mint "$ADV1"); code=000
[ -n "$T" ] && code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 6 -X POST -H "Authorization: Bearer $T" \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://demo-advertiser.example/webhooks/adtech","events":["campaign.budget_depleted","report.completed"]}' \
  "$GW/v1/api/webhooks" 2>/dev/null)
echo "  webhook create HTTP: $code"

echo "▶ [5/8] payouts: give publisher accounts a payout method + run payout-runner…"
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

echo "▶ [6/8] marketplace: a buyer purchases a public listing → grant…"
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

echo "▶ [7/8] audiences: load 1st-party segments (+REAL targetable members) + a 3rd-party data provider for demo advertisers…"
# The demo advertiser (e.g. de6b0145) owns NO segments (the 12 seeded ones live on
# other accounts), so its Audiences tab looks empty. Load a realistic mix per demo
# advertiser: first-party CRM/behavioral/lookalike segments, plus one third-party
# data provider + licensed segments. Idempotent: skip an advertiser that already has any.
#
# HONESTY / TARGETABILITY (the point of this step):
# Members are drawn from the SAME user-id space the demo traffic actually carries,
# so a campaign targeting a segment genuinely matches a real bid request. The id
# the DSP matches on is openrtb.UserKey(user) = User.ID (or the UID2 token) — and
# the per-user Redis inverted index is audience:set:{user_id}:dsp_private, written
# by the mig-078 changelog trigger → cmd/pipeline drainer (the normal pipeline;
# nothing special here). The two recurring id spaces the simulator sends:
#   • synth-user-NNNNNN  — `make demo-loadtest` runs `simulator --user-pool N`
#     (default 2500), which draws ids `synth-user-%06d` from 0..N-1 (serve.go);
#     cmd/seed --synthetic-users uses the identical format. THIS is the pool we
#     seed from, so a post-`demo-loadtest` auction for a member hits the segment.
#   • {persona}-uNNN     — themed personas (UserPool>0) recur under stable ids
#     (`make demo --profile themed`); we also fold a few in so themed traffic hits.
# We do NOT use generate_series synthetic ids any more — those matched no traffic,
# so "Cart Abandoners" was never actually targetable (the segment looked full but
# every real bid missed). size_estimate/match_rate are DERIVED from the real
# member count after insert — no hardcoded 12500/0.62 guesses.
#
# USER_POOL mirrors scripts/demo-loadtest.sh's default so the seeded ids fall
# inside the pool that run carries. Keep them in sync.
AUD_POOL="${USER_POOL:-2500}"
# Per-advertiser member counts (kept modest so the step is fast + idempotent, but
# large enough that a short load run reliably hits several). Clamped to the pool.
HV_N=500; CA_N=300; LA_N=1200; TP_N=900
[ "$HV_N" -gt "$AUD_POOL" ] && HV_N="$AUD_POOL"
[ "$CA_N" -gt "$AUD_POOL" ] && CA_N="$AUD_POOL"
[ "$LA_N" -gt "$AUD_POOL" ] && LA_N="$AUD_POOL"
[ "$TP_N" -gt "$AUD_POOL" ] && TP_N="$AUD_POOL"
i=0
for ADV in $(echo "$ADVS" | head -6); do
  i=$((i+1))
  # Idempotency: skip advertisers that already have REAL targetable members. But
  # an earlier version of this step seeded non-targetable generate_series ids
  # ('aud-%'); detect that legacy residue and re-seed with real members (delete the
  # stale segments first so the changelog trigger tombstones the dead Redis sets).
  have=$(PSQL -tAc "SELECT count(*) FROM audience_segments WHERE account_id='$ADV'" 2>/dev/null | tr -d ' ')
  legacy=$(PSQL -tAc "SELECT count(*) FROM audience_segment_members m JOIN audience_segments s ON s.id=m.segment_id WHERE s.account_id='$ADV' AND m.user_id LIKE 'aud-%'" 2>/dev/null | tr -d ' ')
  if [ "${have:-0}" -gt 0 ] && [ "${legacy:-0}" -gt 0 ]; then
    echo "  ↻ $ADV has legacy non-targetable ('aud-%') members — re-seeding with real ones"
    PSQL -q -c "DELETE FROM audience_segments WHERE account_id='$ADV' AND name IN ('High-Value Customers (CRM)','Cart Abandoners 7d','Lookalike: High-Value','Auto Intenders (3P)','Luxury Shoppers (3P)')" >/dev/null 2>&1 || true
    PSQL -q -c "DELETE FROM data_providers WHERE account_id='$ADV' AND name='Acxiom Licensed Segments'" >/dev/null 2>&1 || true
  elif [ "${have:-0}" -gt 0 ]; then
    echo "  ~ $ADV already has $have segments (real members) — skip"; continue
  fi
  # Deterministic per-advertiser offset into the shared synth-user universe: each
  # advertiser's segments draw a different (but overlapping — realistic) slice, all
  # inside [0, AUD_POOL). user_id = synth-user-%06d of ((offset + g) % AUD_POOL).
  OFF_HV=$(( (i * 131) % AUD_POOL ))
  OFF_CA=$(( (i * 271 + 50) % AUD_POOL ))
  OFF_LA=$(( (i * 97  + 25) % AUD_POOL ))
  PSQL -q >/dev/null 2>&1 <<SQL || true
    -- High-Value Customers (CRM, first-party): real synth-user members the load run carries.
    WITH s1 AS (INSERT INTO audience_segments (id,account_id,name,type,source,status,visibility,created_at,updated_at)
      VALUES (gen_random_uuid(),'$ADV','High-Value Customers (CRM)','first_party','crm_upload','active','dsp_private',now(),now()) RETURNING id)
    INSERT INTO audience_segment_members (segment_id,user_id,account_id,added_at,source)
      SELECT (SELECT id FROM s1),
             'synth-user-'||lpad((($OFF_HV + g) % $AUD_POOL)::text,6,'0'),'$ADV',now(),'crm_upload'
      FROM generate_series(0,$HV_N-1) g
      ON CONFLICT DO NOTHING;
    -- Fold a few themed-persona recurring ids into CRM so \`--profile themed\` traffic also hits.
    INSERT INTO audience_segment_members (segment_id,user_id,account_id,added_at,source)
      SELECT (SELECT id FROM audience_segments WHERE account_id='$ADV' AND name='High-Value Customers (CRM)'),
             p.name||'-u'||lpad(g::text,3,'0'),'$ADV',now(),'crm_upload'
      FROM (VALUES ('dog-lover-mobile'),('cat-lover-mobile'),('coffee-snob-desktop'),('fitness-fan-mobile')) AS p(name),
           generate_series(0,9) g
      ON CONFLICT DO NOTHING;

    -- Cart Abandoners 7d (behavioral/pixel): different slice, TTL-less demo members.
    WITH s2 AS (INSERT INTO audience_segments (id,account_id,name,type,source,status,visibility,created_at,updated_at)
      VALUES (gen_random_uuid(),'$ADV','Cart Abandoners 7d','behavioral','pixel','active','dsp_private',now(),now()) RETURNING id)
    INSERT INTO audience_segment_members (segment_id,user_id,account_id,added_at,source)
      SELECT (SELECT id FROM s2),
             'synth-user-'||lpad((($OFF_CA + g) % $AUD_POOL)::text,6,'0'),'$ADV',now(),'pixel'
      FROM generate_series(0,$CA_N-1) g
      ON CONFLICT DO NOTHING;

    -- Lookalike: High-Value (modeled): a broader slice of the SAME real universe,
    -- so it is genuinely targetable (previously seeded with ZERO members).
    WITH s3 AS (INSERT INTO audience_segments (id,account_id,name,type,source,status,visibility,created_at,updated_at)
      VALUES (gen_random_uuid(),'$ADV','Lookalike: High-Value','lookalike','modeled','active','dsp_private',now(),now()) RETURNING id)
    INSERT INTO audience_segment_members (segment_id,user_id,account_id,added_at,source)
      SELECT (SELECT id FROM s3),
             'synth-user-'||lpad((($OFF_LA + g) % $AUD_POOL)::text,6,'0'),'$ADV',now(),'modeled'
      FROM generate_series(0,$LA_N-1) g
      ON CONFLICT DO NOTHING;

    -- Derive size_estimate + match_rate from the ACTUAL member count (no guesses).
    -- match_rate = share of the addressable pool this segment covers (demo heuristic
    -- over the real members), clamped to a believable ceiling.
    UPDATE audience_segments s SET
      size_estimate = c.n,
      match_rate = round((least(0.95, c.n::numeric / greatest($AUD_POOL,1)))::numeric, 3)
    FROM (SELECT segment_id, count(*) n FROM audience_segment_members GROUP BY segment_id) c
    WHERE s.id = c.segment_id AND s.account_id='$ADV';
SQL
  CNT=$(PSQL -tAc "SELECT coalesce(sum(size_estimate),0) FROM audience_segments WHERE account_id='$ADV'" 2>/dev/null | tr -d ' ')
  echo "  ✔ $ADV: 3 first-party segments + ${CNT:-0} real targetable members (synth-user pool + themed)"
done
ADV1=$(echo "$ADVS" | head -1)
if [ -z "$(PSQL -tAc "SELECT 1 FROM data_providers WHERE account_id='$ADV1' LIMIT 1" 2>/dev/null | tr -d ' ')" ]; then
  # Third-party licensed segments: real members from the SAME synth-user universe
  # (a licensed-data provider's ids are, for the demo, the targetable pool) so the
  # "Auto Intenders / Luxury Shoppers (3P)" segments are genuinely usable at bid
  # time — not zero-member decorations. size derived from the real member count.
  OFF_AI=$(( 311 % AUD_POOL )); OFF_LS=$(( 701 % AUD_POOL ))
  PSQL -q >/dev/null 2>&1 <<SQL || true
    WITH p AS (INSERT INTO data_providers (id,account_id,name,kind,default_party,default_licence,status,scope,created_at,updated_at)
      VALUES (gen_random_uuid(),'$ADV1','Acxiom Licensed Segments','dmp','third','purchased','active','account',now(),now()) RETURNING id),
    ai AS (INSERT INTO audience_segments (id,account_id,name,type,source,status,match_rate,visibility,data_party,provider_id,created_at,updated_at)
      VALUES (gen_random_uuid(),'$ADV1','Auto Intenders (3P)','behavioral','provider','active',0.9,'dsp_private','third',(SELECT id FROM p),now(),now()) RETURNING id),
    ls AS (INSERT INTO audience_segments (id,account_id,name,type,source,status,match_rate,visibility,data_party,provider_id,created_at,updated_at)
      VALUES (gen_random_uuid(),'$ADV1','Luxury Shoppers (3P)','behavioral','provider','active',0.9,'dsp_private','third',(SELECT id FROM p),now(),now()) RETURNING id),
    mi AS (INSERT INTO audience_segment_members (segment_id,user_id,account_id,added_at,source)
      SELECT (SELECT id FROM ai),'synth-user-'||lpad((($OFF_AI + g) % $AUD_POOL)::text,6,'0'),'$ADV1',now(),'provider'
      FROM generate_series(0,$TP_N-1) g ON CONFLICT DO NOTHING RETURNING segment_id)
    INSERT INTO audience_segment_members (segment_id,user_id,account_id,added_at,source)
      SELECT (SELECT id FROM ls),'synth-user-'||lpad((($OFF_LS + g) % $AUD_POOL)::text,6,'0'),'$ADV1',now(),'provider'
      FROM generate_series(0,$TP_N-1) g ON CONFLICT DO NOTHING;
    UPDATE audience_segments s SET size_estimate = c.n
    FROM (SELECT segment_id, count(*) n FROM audience_segment_members GROUP BY segment_id) c
    WHERE s.id = c.segment_id AND s.account_id='$ADV1' AND s.data_party='third';
SQL
  echo "  ✔ $ADV1: 1 third-party data provider + 2 licensed segments (real members, size derived)"
fi
# Force a drain+reconcile so the new members are bid-eligible in Redis immediately
# (the pipeline's 3s delta poll would catch up on its own; this just makes the
# members instantly targetable). The audience cache writer lives in cmd/pipeline
# (:8087, /debug/audience/refresh). Best-effort from the host; if the pipeline port
# isn't bridged, exec into the pod.
PIPE_POD=$(kubectl get pods -n adtech -l app=pipeline -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
if [ -n "$PIPE_POD" ]; then
  kubectl exec -n adtech "$PIPE_POD" -- sh -c 'wget -q -O- --post-data="" http://localhost:8087/debug/audience/refresh 2>/dev/null || true' >/dev/null 2>&1 \
    && echo "  ✔ audience cache drained+reconciled (members bid-eligible now)" \
    || echo "  ~ audience refresh best-effort (pipeline drains on its 3s poll regardless)"
fi

echo "▶ [8/8] external partners: register demand/supply partners + walk the onboarding lifecycle…"
# The "External partners" registry (partners table) is a SEPARATE system from the
# exchange's bid demand (dsp_endpoints config) — staff onboard DSP/SSP partners
# through pending→sandbox→certified→active. Seed a spread so the tab shows the
# lifecycle. Platform-global; needs partners:manage (admin token). Idempotent:
# skip if any partner already exists. The registry enforces transition ORDER, so
# walk each step (with a brief settle to avoid a compare-and-swap race).
#
# IMPORTANT: no DEMO DSP is left `active`. An active DSP partner is merged into the
# exchange's LIVE auction fan-out (feature #112), so an active partner with a
# placeholder `.example` endpoint would be dialed on every auction and fail DNS —
# noise + wasted latency for zero bids. The one `active` partner here is an SSP
# (Nova), which the fan-out excludes (kind='dsp' only). The DSPs stay in onboarding
# states (paused/certified/sandbox/pending), which is also the honest picture:
# they're demo onboarding records, not live bidders.
STAFF=$(curl -s --max-time 6 -X POST "$GW/v1/auth/token" -H 'Content-Type: application/json' -d '{}' \
  | python3 -c 'import sys,json;print(json.load(sys.stdin).get("token",""))' 2>/dev/null)
PCOUNT=$(curl -s --max-time 6 "$GW/v1/api/partners" -H "Authorization: Bearer ${STAFF:-}" 2>/dev/null \
  | python3 -c 'import sys,json;print(len(json.load(sys.stdin)))' 2>/dev/null || echo 0)
if [ -z "${STAFF:-}" ]; then
  echo "  ⚠ no admin token (debug endpoints off?) — skipping partners"
elif [ "${PCOUNT:-0}" -gt 0 ]; then
  echo "  ~ $PCOUNT partners already registered — skip"
else
  regpartner(){ # 1=name 2=kind 3=endpoint 4=channels-json 5=target-status
    local id steps s
    id=$(curl -s --max-time 8 -X POST "$GW/v1/api/partners" -H "Authorization: Bearer $STAFF" \
      -H 'Content-Type: application/json' \
      -d "{\"name\":\"$1\",\"kind\":\"$2\",\"endpoint_bid\":\"$3\",\"channels\":$4,\"formats\":[\"display\",\"video\"],\"timeout_ms\":120,\"openrtb_version\":\"2.6\",\"contact_tech\":\"ops@example.com\"}" 2>/dev/null \
      | python3 -c 'import sys,json;print(json.load(sys.stdin).get("id",""))' 2>/dev/null)
    [ -z "$id" ] && { echo "  ⚠ register failed: $1"; return; }
    case "$5" in sandbox) steps="sandbox";; certified) steps="sandbox certified";; paused) steps="sandbox certified paused";; active) steps="sandbox certified active";; *) steps="";; esac
    for s in $steps; do
      curl -s -o /dev/null --max-time 6 -X POST "$GW/v1/api/partners/status" -H "Authorization: Bearer $STAFF" \
        -H 'Content-Type: application/json' -d "{\"id\":\"$id\",\"status\":\"$s\"}" 2>/dev/null
      sleep 0.3
    done
    echo "  ✔ $1 ($2) → ${5:-pending}"
  }
  regpartner "Acme Exchange DSP"   dsp "https://bid.acme-dsp.example/openrtb" '["display","video","native"]' paused
  regpartner "Zenith Programmatic" dsp "https://rtb.zenith.example/bid"       '["display","video"]'          certified
  regpartner "Coinflip Media DSP"  dsp "https://bid.coinflip.example/rtb"     '["display"]'                  sandbox
  regpartner "Nova Supply SSP"     ssp "https://ssp.nova.example/req"         '["display","video","audio"]'  active
  regpartner "Horizon Bidder"      dsp "https://bid.horizon.example/openrtb"  '["video"]'                    pending
fi

cat <<EOF

✔ Demo data generated. Now populated in the portals:
  • Advertiser → Billing → Invoices        ($GW/portal/advertiser)  [3 advertisers on invoiced terms]
  • Advertiser → Conversions (pixels) + Team → Webhooks
  • Advertiser → Marketplace → Purchased (grant from a buyer purchase)
  • Publisher  → Earnings → Payouts        ($GW/portal/publisher)
  • Marketplace earnings no longer shows stale/orphaned rows
  • Staff     → External Partners          ($GW/portal/staff)  [5 partners across the lifecycle]
EOF
