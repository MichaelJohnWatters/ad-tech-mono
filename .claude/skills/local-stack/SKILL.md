---
name: local-stack
description: Check and recover the local stack (OrbStack k8s + Tilt) before/after verification work. Use when service ports return connection-refused, kubectl errors, traffic errors en masse, or after the machine has slept (which suspends the OrbStack VM). Also covers seeding fresh data.
---

# Local stack: health check + recovery

The local stack is **OrbStack built-in k8s** (namespace `adtech`, context `orbstack`)
orchestrated by **Tilt**, with port-forwards to `localhost`. The #1 gotcha:
**machine sleep suspends the OrbStack VM**, which drops every port and the k8s API.

## Inputs

- **k8s:** `kubectl get nodes`, `kubectl -n adtech get pods`.
- **Recover a stopped VM:** `orb start` (then wait for the API, then for pods `1/1`).
- **Service health:** `curl -fsS -o /dev/null -w '%{http_code}' http://localhost:<PORT>/healthz`.
  Ports: gateway 8080, exchange 8081, dsp-internal 8082, dsp-competitor1 8089,
  dsp-competitor2 8090, tracker 8083, ssp 8084, adserver 8085, reporting 8086,
  publisher-adserver 8088.
- **Warm-cache refresh** (after a reseed): `curl -sX POST http://localhost:<PORT>/debug/cache/refresh`
  on 8082, 8089, 8090 (DSPs), 8084 (SSP), 8085 (adserver), 8086 (reporting).
- **Data commands:** seed = `make demo` (or `bash scripts/demo.sh`); full wipe+reseed =
  `make reset` (truncates Postgres+ClickHouse+Redis, reseeds, fires all-format traffic).

## Health check

```
kubectl get nodes                                  # k8s API reachable?
kubectl -n adtech get pods | grep -vE '1/1|2/2'    # any non-ready pods? (empty = all good)
for p in 8080 8081 8084 8088 8083; do curl -fsS -o /dev/null http://localhost:$p/healthz && echo "$p ok" || echo "$p DOWN"; done
```

## Recovery (VM stopped / ports refused)

Symptom: `kubectl` → `connection to 127.0.0.1:26443 refused`, or every service port
returns `000`/refused.

1. `orb start`
2. Wait for the API: loop `kubectl get nodes` until it succeeds.
3. Wait for pods: loop until `kubectl -n adtech get pods | grep -c '1/1'` reaches the
   full count (~29).
4. Wait for Tilt to re-establish port-forwards: loop `curl healthz` on 8080/8081/8084/8088.
5. **Event pipeline:** after an OrbStack bounce the async pipeline
   (tracker→NATS→reporting→ClickHouse) reconnects cleanly on a **full** pod cycle
   (which the restart does); mid-session single-pod bounces sometimes don't. If
   impressions fire but don't reach ClickHouse, that's the tell.

## After a reseed / reset

- Truncating + reseeding leaves the DSP/SSP/adserver **warm caches stale** — refresh
  them (see Inputs) or every auction no-bids off an empty cache.
- `make reset` fires its own baseline traffic while caches/router are still warming, so
  a few cold-start errors/no-fills in *that* run are normal — a controlled run right
  after should be clean (0 errors). Verify with the `verify-pipeline` skill.
- An auto-triggered e2e run can truncate seed data — re-`make demo` if accounts/logins vanish.
