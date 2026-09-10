# pkg/privacy - Consent & Opt-Out

The consent decision engine. One stateless verdict (`Evaluate`) gates every
user-data path — DSP bidding, SSP capture, tracker pixels, ARA — plus the
3-level opt-out model behind it.

## Key Entry Points

- `Evaluate(Signals) Decision` (`consent.go`) — hot-path, allocation-free
  verdict combining the platform opt-out registry level with inbound OpenRTB
  regulatory signals (GDPR/TCF, US Privacy, COPPA, GPC, GPP). Returns
  `{Bid, Personalise, Reason}`. Precedence is documented on the function.
- `SignalsFromQuery(get, secGPCHeader)` (`consent.go`) — builds `Signals` from
  ad-tag/pixel query params + the `Sec-GPC` header. Shared by SSP behaviour
  capture and the tracker's retargeting pixel so the capture-time gates can't drift.
- `AllowsResidency(reqRegion, homeRegion)` / `AllowsUserData(decision, reqRegion, homeRegion)`
  (`residency.go`) — data-residency gate (PLAN #111). `AllowsUserData` = consented
  `Personalise` AND the request's `regs.ext.data_residency` (empty = none) matches
  this deployment's `platform.region`. The SSP gates ALL user-data emission through
  it (`requestStoresUserData`); empty region on either side = no constraint.
- `OptOut` + `LevelFromInt` (`consent.go`) — the warm-cache row for the user
  opt-out registry; loaded by `pkg/store/postgres/optouts.go` (`OptOutLoader`)
  into `pkg/cache/warm` caches (DSP), invalidated via
  `events.SubjectCacheInvalidateOptOuts`.
- `GPPOptOut(gpp, gppSID)` (`gpp.go`) — minimal IAB GPP decode; only the US
  National section (id 7) sale/sharing/targeted-ad opt-out bits.
- `Manager` (`privacy.go`) — in-memory opt-out/deletion machinery for the
  3-level model. NOT the production intake: the gateway
  (`cmd/gateway/privacy.go`) writes `opt_out_registry` directly and
  `cmd/privacy-delete` runs the Level 3 purge.

## Invariants & Gotchas

- **Personalisation is gated on `Evaluate().Personalise` — everywhere.** Any
  path touching user data (DSP audience targeting, SSP segment pass-through,
  identity observe, tracker `/v1/t/rt`, ARA registration) must check it first.
- **Regulatory signals downgrade to contextual, never no-bid.** Only a registry
  opt-out Level ≥ 2 produces `Bid: false`. A blanket no-bid would silently drop
  all EU/child/CCPA-opt-out traffic.
- **Malformed input = no signal.** A bad US Privacy string or undecodable GPP
  section never fabricates an opt-out (`usPrivacyOptOut`, `GPPOptOut`).
  `LevelFromInt` clamps out-of-range levels to `LevelNone`.
- **No signal at all = full consent** (`CheckConsent`, `Evaluate` default).
- GPP US state sections 8–12 are recognised but not decoded — safe only because
  GPP is downgrade-only; cross-check bit offsets against IAB test vectors before
  any strict (reject) enforcement.
- `opt_out_registry` is **platform-global** (no account scope — an opt-out
  applies platform-wide); `OptOutLoader` uses `QueryPlatform`, no tenant filter.
- This package imports no NATS. The opt-out/deletion event subjects
  (`adtech.privacy.opt_out` / `deletion_requested` / `deletion_completed`) live
  in `pkg/events`, published by the gateway intake and `cmd/privacy-delete`.

## Used By

- `cmd/dsp` — opt-out warm cache + `Evaluate` in the bid handler (no-bid or
  strip behavioural targeting)
- `cmd/ssp` — behaviour-capture consent gate (`behaviour.go`), identity-observe
  gate, and regs pass-through so the DSP's `Evaluate` sees real inputs
- `cmd/tracker` — retargeting pixel gate + ARA source registration gate (`ara.go`)
- `pkg/store/postgres/optouts.go` — registry loader

## Testing

In-package unit tests (`consent_test.go`, `gpp_test.go`, `privacy_test.go`);
simulator personas exercise every `Evaluate` branch
(`pkg/simulator/request/persona.go`).

## Architecture Details

See `docs/PLAN.md` → "Privacy and Consent", "User Opt-Out and Data Deletion
System", "Privacy in Audience Management".
