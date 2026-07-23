# ADR 0008 — Secure + mappable audience ingestion (PGP decrypt, custom mappings)

**Status:** Accepted (2026-07-23)
**Extends:** ADR 0007 (unified audience ingestion). Both features slot into the
existing `pkg/ingest` pipeline: `stage → [PGP-decrypt] → strict decode → [apply
saved mapping] → validate (any fail → reject) → import id_value/id_type only`.

## Context

Data providers deliver audience files that are (a) often **PGP-encrypted** for
transport security, and (b) in **arbitrary column layouts** we don't control.
Today ingestion assumes plaintext + a fixed set of id column names (or an inline
manifest mapping). Two additions:

## Decision

### 1. PGP decrypt-on-ingest (platform keypair)
- **One platform-wide keypair.** Providers encrypt files to the platform **public
  key**; the platform decrypts on ingest with the **private key**. (Industry norm
  for secure file drop; per-account keys are out of scope — revisit if a provider
  demands isolation.)
- **Private key lives in the secrets store** (`secrets` table + `pkg/secrets`
  cache), new purpose `pgp_private`, `owner='platform'`, with the same
  active/rotating/revoked model as `jwt_signing` (so the public key can rotate
  without breaking in-flight files — decrypt tries every non-revoked private key).
  Prod supplies it via SOPS/K8s Secret; local `cmd/seed` generates one if absent.
- **Public key is non-secret** — served at `GET /v1/api/audiences/pgp-key`
  (armored + fingerprint).
- **Decrypt slots before decode**: `ingest.Processor` gains the keyring; both
  `Process` and `ValidateSample` decrypt the body first if it is OpenPGP (armor
  `-----BEGIN PGP MESSAGE-----` or binary packet magic), else pass through. A file
  encrypted to an unknown key → reject with a clear reason.
- **UI**: the advertiser upload screen + drop-zone docs show an "encrypt to this
  key" panel (public key, fingerprint, copy button); `.pgp`/`.gpg`/armored files
  auto-decrypt on ingest.
- Library: `github.com/ProtonMail/go-crypto/openpgp` (maintained; `x/crypto/openpgp`
  is frozen).

### 2. Custom field mappings ("connectors")
- **Tenant-scoped, named, saved mappings.** New `audience_mappings` table
  (`account_id` FK, `name`, `mappings JSONB` = their column → our canonical field,
  `id_type`, timestamps) with an **RLS tenant policy** — an account sees only its
  own mappings.
- **Targets are limited to the fields we consume** (`id_value` required, `id_type`;
  extend later if we start using more). The builder offers only these + "ignore",
  so a provider **cannot** map to fields we don't use — and at ingest we project to
  only the mapped canonical columns. Unmapped columns are dropped and never stored
  (`profile_signals` already persists only `id_value`/`id_type`). This is the
  "prevent needless data" guarantee.
- **Build via a sample upload**: `POST /v1/api/audiences/mappings/sample` decodes a
  small sample with the strict parser and returns its detected (lowercased)
  columns; the UI maps each → canonical/ignore; save via
  `POST /v1/api/audiences/mappings`. A mapping with no column → `id_value` is
  invalid.
- **Apply at upload**: the upload accepts `mapping_id`; the handler loads the
  tenant's mapping and sets `SegmentSpec.FieldMappings` from it. Drop-zone
  providers reference a mapping **by name** in their manifest.
- Auth: mapping CRUD is JWT-gated + tenant-scoped (RLS + explicit `account_id`).

## Consequences
- **Positive**: secure transport (PGP) + arbitrary-format onboarding without
  bespoke code per provider; data minimisation is structural (only consumed
  columns are mappable/stored); reuses ADR 0007's one processor + strict validate.
- **Cost/risk**: the platform now holds a PGP private key (secrets-store blast
  radius; mitigated by rotation + SOPS in prod). A new dependency
  (ProtonMail/go-crypto). Mapping targets are intentionally narrow — widening them
  is a deliberate future change, not provider-configurable.

### 3. Ingest completion emails (optional)
- On terminal state (done/failed) an ingest job optionally emails the outcome:
  **primary recipient = the uploader** (email resolved from `team_members` by the
  JWT `UserID`); **additional recipients optional** (an `additional_emails` field on
  the upload, or `notify_emails` in a drop-zone manifest). Stored as
  `audience_ingest_jobs.notify_emails TEXT[]` at enqueue.
- Sent via `pkg/email.Sender` (SMTP/Mailpit, memory fallback) — inline jobs notify
  from the gateway, async jobs from the pipeline worker, after MarkDone/MarkFailed.
  Subject "Audience upload '<name>' succeeded/failed"; body = members added +
  match rate on success, or the reject reason on failure.

## Migration plan
1. **PGP** — secrets purpose + migration (CHECK enum), keygen in seed, decrypt in
   `pkg/ingest`, public-key endpoint, upload-screen panel.
2. **Custom mappings** — table + RLS, store, sample/CRUD endpoints, `mapping_id` on
   upload, mapping-builder UI section.
3. **Completion emails** — `notify_emails` column, capture at enqueue, send on
   terminal state from both the gateway (inline) and pipeline worker (async).
