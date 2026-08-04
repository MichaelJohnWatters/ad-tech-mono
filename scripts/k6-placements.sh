#!/usr/bin/env bash
# k6-placements.sh — emit "tagid|publisherID|domain,..." pairs for the k6
# exchange load test (tests/k6/exchange-load.js) from the live seeded world,
# so raw OpenRTB requests reference real placements and can actually win.
# Kept out of the Makefile: make 3.81 (the macOS default) corrupts recipes
# that continue a double-quoted command across backslash-newlines.
set -euo pipefail
kubectl -n adtech exec postgres-0 -- psql -U adtech -d adtech -tAc \
  "SELECT string_agg(x.pair, ',') FROM (
     SELECT p.id::text || '|' || p.publisher_id::text || '|' || pb.domain AS pair
     FROM placements p JOIN publishers pb ON pb.id = p.publisher_id
     WHERE p.status = 'active' AND p.format = 'display' LIMIT 8) x" 2>/dev/null
