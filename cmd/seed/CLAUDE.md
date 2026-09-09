# Seed Job

One-shot idempotent job that UPSERTs the YAML seed profiles into Postgres and uploads
themed SVG creatives + sample media to Minio/S3 (best-effort — no `s3.endpoint` →
creatives fall back to inline HTML). Seeds `profiles/{dsps,publishers,deals,direct-sold}/*.yaml`
(**`--profile` is a label only; ALL YAMLs in each dir load**) plus the feature baseline
(providers/segments/agency/rates/house ads), dev secrets (`pkg/secrets` cipher; plaintext when
`SECRETS_ENCRYPTION_KEY` unset), dev logins (`admin@adtech.local`/`admin`), $10k advertiser
balances, G7 conversion keys, ads.txt rows (`adtech.local`/`adtech-exchange`), and the
**$5 overspend canary** (`canary.go`). Additive tiers never truncate the small world:
`--big-world-*`, `--synthetic-*` (timeout auto-scales — each membership insert fires the
mig-078 changelog trigger).

## How it runs

- Host: `make seed` / `seed-minimal` / `seed-stress`; wrapped by `make demo` / `make reset`.
  In-cluster: subprocess of the gateway's debug-gated `POST /dev/reset-and-reseed` (gateway
  image bakes `/seed` + `/profiles`). Command map: `k8s/CLAUDE.md` → "Seed / reset / demo data".

## Gotchas

- Every UUID derives from its YAML external key (`pkg/idgen.Derive`); re-runs UPSERT in
  place and any runtime code can regenerate the same ID from the same key.
- **Needs the owner/admin DB URL** — cross-tenant inserts the RLS-flipped `adtech_app` role
  can't do; the reseed endpoint passes the gateway's `database.admin_url` /
  `DATABASE_ADMIN_URL` to the subprocess as its `DATABASE_URL`.
- `seed.creatives_url_base` / `seed.landing_url_base` are Raw handles (`pkg/config/keys/seed.go`);
  asset/landing bases must be **browser**-reachable URLs, not in-cluster ones.
- `main.go` ordering: big-world runs before `SeedDevUsers`/`SeedAdvertiserBalances` so its
  accounts get logins + funds; dev secrets skip if an active row exists.

## Pointers

- Profile shapes: `profiles/` YAMLs; API-built big worlds: `bigworld_test.go` (`BIGWORLD=1`);
  realistic data on top: the `/generate-data` skill.
