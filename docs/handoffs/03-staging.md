# Session: first deployment off the laptop (staging arc)

## Context
The platform has ONLY ever run on Rancher Desktop k3s locally. The chart
(k8s/helm/adtech) already has values-staging.yaml / values-prod.yaml shapes:
real S3 (no Minio), SOPS secrets, cert-manager TLS, no localExpose. All
images are locally built (scripts/stack-images.sh) — no registry push
pipeline exists yet. Local perf is proven (docs/perf/RESULTS.md; auction
p95 23-75ms at 50-110rps); a real node answers what the laptop can't:
honest CPU (no simulator sharing cores), real network in fanout, real
ads.txt crawling on a real domain.

## Goal (incremental — stop and report between milestones)
1. Image publishing: pick a registry (ghcr.io simplest), tag+push pipeline
   (ties into CI if it exists by now).
2. Provision a single k3s node (any cheap VPS; user chooses provider —
   ASK before creating any paid resources).
3. Deploy with values-staging.yaml; fix what breaks (expect: secrets
   sourcing, S3 endpoint/domain config, ingress/DNS, resource fits).
4. Run the perf protocol remotely (the simulator can run ON the node or
   from the laptop — note which; comparability caveats in the ledger note).

## Read first
k8s/CLAUDE.md, values-staging/prod.yaml, memory: project_helm_migration,
project_security_hardening (image pinning, adtech_app role), CLAUDE.md.

## Constraints
- NOTHING real-credential committed; SOPS or env-injection only.
- Ask before any spend / external account creation.
