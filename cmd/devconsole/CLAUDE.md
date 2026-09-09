# Devconsole (Host Tool)

HOST-side dev-loop UI — the deliberate Tilt-dashboard replacement. Never deployed
to K8s; runs on the host because image builds need the local Go toolchain +
docker socket. Cluster-side ops (pod restarts, cron triggers, log tail) live in
the staff portal's Ops section instead — do not grow that functionality here.

## How to run

- `make devconsole` → http://localhost:8099 (`go run ./cmd/devconsole`)
- Binds `127.0.0.1:8099` only — it shells out to `make`/`go`, so never expose it.

## What it does

- Per-service **Build + deploy** buttons → streams `make deploy SVC=x` output
  live into the page (chunked plain text). The `services` list in `main.go` is
  hand-synced with `scripts/stack-images.sh` (GENERIC + the gateway/transcoder/
  reporting special-case builds; check for drift — notifications / audience-rt /
  invoice-runner / account-closeout are in the script but missing here today);
  "dsp" fans out to all three DSP deployments.
- Stack buttons (`stackActions`): `stack-up`, `stack-images` (build all),
  `seed` (`go run ./cmd/seed --profile standard`), `demo`.
- Quick links to gateway/grafana/jaeger/minio/mailpit.

## Endpoints

- `GET /` — the single embedded page (`console.html`, `go:embed`)
- `POST /run/deploy/{svc}` — 400 on unknown service
- `POST /run/action/{name}` — one of the `stackActions` make targets

## Gotchas / rules

- **One action at a time** (mutex; a second concurrent action gets 409) — build
  output must never interleave. Keep it that way.
- **Thin by design**: it is a face on the exact commands you'd type, not an
  orchestrator. New capability = a new make target it calls, not logic here.
- Deliberately does NOT use `pkg/routes`/`pkg/config` — it's a host binary
  outside the service conventions (stdlib only: no /healthz, no lifecycle,
  no NATS).
- Add a service here when you add one to `scripts/stack-images.sh`.

## Pointers

- `docs/PLAN.md` -> "Developer Tools" (the console pair: staff-portal Ops
  section = in-cluster acting surface; this = host build/deploy surface)
- `k8s/CLAUDE.md` -> "Dev loop (Makefile)" for what the make targets do
