#!/usr/bin/env bash
#
# reset.sh — ONE command for a fresh run. Wipes ALL local data (Postgres tenant
# tables + ClickHouse analytics + Redis counters), then re-populates via
# demo.sh. Config/secrets/schema are preserved, so no gateway/DSP restart is
# needed. Run via `make reset` or the Tilt "reset" button.
#
# Uses kubectl against the OrbStack cluster (namespace: adtech).
set -euo pipefail
cd "$(dirname "$0")/.."
NS="${DEMO_NAMESPACE:-adtech}"

echo "▶ [reset 1/4] truncating Postgres tenant tables…"
kubectl -n "$NS" exec postgres-0 -- psql -U adtech -d adtech -q -c "
TRUNCATE TABLE
  line_item_creatives, publisher_line_item_creatives, targeting_rules,
  line_items, publisher_line_items, creatives, insertion_orders, placements,
  deals, publishers, audience_segment_members, audience_segments,
  opt_out_registry, identity_graph, budget_reservations, reservation_context,
  campaign_committed_spend, ledger_entries, invoices, payouts, adjustments,
  advertiser_balances, topups, api_keys, team_members, accounts
RESTART IDENTITY CASCADE;" >/dev/null

echo "▶ [reset 2/4] clearing ClickHouse analytics…"
for t in $(kubectl -n "$NS" exec clickhouse-0 -- clickhouse-client -q "SHOW TABLES FROM adtech" 2>/dev/null); do
  kubectl -n "$NS" exec clickhouse-0 -- clickhouse-client -q "TRUNCATE TABLE IF EXISTS adtech.\`$t\`" 2>/dev/null || true
done

echo "▶ [reset 3/4] flushing Redis (budgets, freq caps, dedup)…"
kubectl -n "$NS" exec deploy/redis -- redis-cli FLUSHDB >/dev/null 2>&1 || true

echo "▶ [reset 4/4] re-seeding + re-populating…"
exec bash scripts/demo.sh
