# Platform UI Build Plan (living / self-expandable)

**Status:** Living document. Started 2026-07-02. Grounded in a code/API/auth investigation.
**Related:** `docs/UI_DESIGN_AUDIT.md` (design-debt + component-library groundwork),
`pkg/auth/auth.go` (personas/RBAC), `pkg/routes/routes.go` (API surface),
`docs/PLAN.md` → "Signup and Onboarding" / "Dashboard".

## Purpose

Build the customer-facing UI for the platform — persona portals for **advertisers, publishers,
agencies, account managers (staff), and admins** — on the existing Go-templates + HTMX + Tailwind
stack (no JS build). This doc is **self-expandable**: every screen/area is a card with the same
shape, and each card carries **prompts** (the questions/checklists to fill in) so we can flesh out
one section at a time without re-planning the whole thing.

---

## How to use / expand this doc

Every UI area (a persona portal, or a screen within it) is described with the **Area Card
template** below. To add or deepen an area: copy the template, fill what you know, leave the
prompts as open questions. "Status" ratchets: `⬜ not started → 🟡 in progress → ✅ shipped`.

### Area Card template (copy me)

```
### <Area name>  <status emoji>
- **Persona / permission gate:** <account_type:role → required permission(s)>
- **Job-to-be-done:** <the one sentence a user would say>
- **Screens:** <list — each becomes its own card when it grows>
- **APIs:** <exists: routes.X | GAP: needs new endpoint (see API Gaps register)>
- **Components used:** <from the component library>
- **Data (RLS-scoped tables):** <tables read/written>
- **Delivery checklist:** [ ] handler+route  [ ] template  [ ] HTMX fragments  [ ] permission gate  [ ] empty/error/loading states  [ ] tests
- **Prompts (fill as we go):**
  - What are the top 3 actions on this screen?
  - What's the empty state / first-run experience?
  - Which fields are editable inline (HTMX) vs in a modal?
  - What real-time signals matter (spend, fill, pacing) and how fresh?
  - What can go wrong and how do we surface it (toast + undo, not confirm dialogs)?
- **Open questions:** <...>
```

---

## Current state (investigation summary)

**Stack (keep):** Go `html/template` + HTMX + Tailwind (CDN) + a small `dict` FuncMap; a
`templateManager` with dev live-reload (`cmd/gateway/templates.go`). No Node/bundler.

**What exists (all dev-only, no auth gate):**
- `/dev/console` (aka `/dev/config-manager`) — the **operator console** (`config/manager.html`,
  ~1.5k lines): tabbed (Overview/Services/Pods/Config→Live/Secrets/Static/History), fuzzy search,
  service pills, per-pod rows, toasts, inline history. **This is the UX bar + reuse template.**
- `/dev/publisher-simulator` (`simulator/minimal.html`, ~3.7k lines) — real auctions, trace
  timeline, observability quick-links. Visually rich but ~250 inline styles.
- `/dev/trace-explorer` (`trace/explorer.html`) — trace lookup + batch reconciliation.
- `/` dashboard, `/dev/landing/{brand}` demo pages.
- Component partials already exist: `components/{button,modal,toast}.html`. `layout.html` is
  **dead code** (not wired). Nav/theme/tailwind config are **duplicated ~4×** (see design audit).

**The gap:** there is **no customer-facing portal** — no advertiser/publisher/agency/staff app,
no auth-gated app shell, no per-role navigation, no real login (dev bypass grants admin claims).

**Personas + RBAC (from `pkg/auth/auth.go`):** 5 account types × 6 roles with a full
`defaultPermissions` map; multi-tenancy via `CanAccessAccount` + RLS (`X-Account-ID` header →
`app.current_account_id`); agencies carry `ManagedAccounts[]` in the JWT.

---

## Foundation workstream **F** — Theme + Component Library + App Shell + Auth  🟡

The enabler for everything else. **Build this first** — every portal composes from it. Detailed
because you asked to "build a theme + component library."

### F1. Design tokens (single source of truth)  ✅
> Shipped: `web/static/tokens.css` is the canonical token file (`--brand-*` + `-rgb` twins for
> Tailwind alpha, surface/text scales light+dark, `--service-*` palette incl. billing/nats,
> fonts, radii). `theme.css` @imports it and keeps only component classes; `head-meta.html`
> links it and maps every Tailwind colour → the vars (`rgb(var(--x-rgb) / <alpha-value>)` for
> semantics so `bg-brand/20` works). Service palette reconciled to the simulator/head-meta set
> (`exchange #eab308, dsp #14b8a6, …`) — trace-explorer's stale `.trace-service` colours updated;
> the sim's `TRACE_SVC_COLOR` JS keeps a documented hex copy (feeds `hexToRgba()`).

Kill the 4× duplication (audit problems 1–7). One token set, consumed by both Tailwind pages and
CSS-var pages.
- **Prompts (remaining):** typography scale (display/body/mono)? density (comfortable vs compact)?

### F2. App shell  🟡 (started)
> Shipped: `partials/app-sidebar.html` (data-driven nav, active highlight, theme toggle) and
> **portal design mocks** at `/dev/portal/advertiser` (composing sidebar + stat/sparkline/table/
> badge/modal/drawer on demo data). Remaining: role-aware nav filtering (needs F4 claims),
> topbar/user-menu partial, breadcrumb wiring, responsive, account switcher.

Wire `layout.html` for real: `<head>` partial (tokens + HTMX + theme toggle), a **role-aware**
top nav + left sidebar, breadcrumb slot, toast container, content block. One nav, driven by the
user's permissions (hide what they can't see).
- **Deliver:** `layout.html` + `partials/{nav,sidebar,head,breadcrumb}.html`; pages become
  `{{ template "layout" . }}` with a content block. Nav items are data (`[]NavItem{Label, Href,
  Perm, Icon}`) filtered by `auth.HasPermission`.
- **Prompts:** IA — top-level sections per persona? sidebar vs top-nav split? account switcher
  placement (agencies)? mobile/responsive scope?

### F3. Component library (Go template partials)  🟡 (started)
> Shipped: `stat`, `badge`, `pill`, `empty-state`, `form-field`, `card` (start/end),
> `table` (start/end) — joining the existing `button`/`modal`/`toast`; a `slice` FuncMap
> helper; and a live **showcase at `/dev/components`** (parse + render tested). Remaining:
> tabs/subtab-bar, search-input, drawer, chart/sparkline, wizard, breadcrumb, pagination, toggle.

Reusable, parameterised via `dict`. Start from the three that exist; add the rest. Each is a
partial + (if needed) a `/static/*.js` behaviour + a doc snippet.
- **Have:** `button`, `modal`, `toast`.
- **Build:**
  - `table` / `data-table` (sortable header, empty state, row actions, HTMX pagination)
  - `card` / `panel` (metric/KPI card, status panel)
  - `stat` (big number + delta + sparkline)
  - `tabs` + `subtab-bar` (the Live/Secrets/Static/History pattern)
  - `pill` (single/multi-select filter chips)
  - `search-input` (with fuzzy/strict mode toggle)
  - `form-field` (label + input + help + error), `select`, `checkbox-grid`, `toggle`
  - `badge` / `status-chip` (live/paused/ended/pending review…)
  - `drawer` (side panel for detail/edit), `tooltip`, `pagination`, `breadcrumb`
  - `chart` — inline-SVG sparkline + bar/line (no chart lib; small helper) for time series
  - `empty-state`, `skeleton`/`loading`, `banner` (drain/notice)
  - `wizard` (multi-step onboarding)
- **Interaction rules (house style):** HTMX for partial swaps + inline edit; **toast + undo, never
  `confirm()`/`prompt()`**; optimistic UI where safe; no inline `style=` (lint-guarded).
- **Prompts:** which components does the first portal (advertiser) actually need? what's the
  minimal set to ship Phase 1? chart approach — inline SVG vs a 3KB lib?

### F4. Real auth + session  🟡 (login shipped)
> Shipped (F4a): cookie-based sessions (`middleware.Auth` reads the `adtech_session`
> httpOnly cookie), a real login flow (`POST /v1/auth/login` → bcrypt-verify `team_members`
> → JWT → cookie → persona redirect; `/login` page; `/v1/auth/logout`), and a seeded dev
> admin (`admin@adtech.local` / `admin`). Additive — the dev bypass (empty signing key →
> admin) is untouched, so it only becomes the real gate when `gateway.require_auth` + a
> `jwt_signing` secret are set. Remaining (F4b): gate the portal routes on claims, pass
> `*auth.Claims` to templates for **role-aware nav** (filter by `HasPermission`), agency
> act-as, password reset (Mailpit).

Today auth is bypassed (empty signing key → admin claims). A customer UI needs real login.
- **Deliver:** login page → verify `team_members.password_hash` (bcrypt) → issue JWT
  (`middleware.CreateToken`) → httpOnly session cookie → gateway `Auth` middleware reads it →
  templates receive `*auth.Claims` for nav/permission gating. Logout, refresh, "act as account"
  for agencies (switch `AccountID` within `ManagedAccounts`). Signup deferred to per-persona
  onboarding wizards.
- **Prompts:** session cookie vs Authorization header for the browser? refresh strategy? password
  reset + email (Mailpit exists)? MFA scope? real signup now or invite-only?

### F5. Guardrails  ✅
> Shipped: `scripts/audit-ui.sh` (ratcheting budgets — inline styles 290, confirm/prompt 0,
> arbitrary hex 0, alert 0) now runs in CI via `.github/workflows/ci.yml` (audit-ui + build +
> unit tests on push/PR — the repo's first CI). manager.html's last `confirm()` became a
> two-step arm/confirm delete. `web/templates/components/README.md` documents the inventory,
> compose-don't-copy rule, house style, and how to add a component.

CI/lint: grep-fail on `style="`, `confirm(`, `prompt(` in `web/templates`; a components README so
new screens compose instead of copy-paste.

**F delivery order:** F1 tokens → F2 shell → F3 components (the set Phase 1 needs) → F4 auth →
F5 guardrails. F1–F3 are additive/low-risk (new files; existing dev pages migrate opportunistically).
F4 is the gate for real portals.

---

## Persona portals (each a self-expandable Area Card)

> Fill these in as we build. APIs marked **GAP** need backend work (see API Gaps register).

### Advertiser portal  🟡 (MVP live)
> Shipped (Phase 1 MVP): `/portal/advertiser` (alias `/dev/portal/advertiser`) is a real
> 4-section portal on real APIs — Dashboard (today KPIs + 7-day spend trend from the reports
> query), Campaigns (list/create/pause/edit via `/v1/api/campaigns` → DSP CRUD), Reports (query
> console on `/v1/api/reports`), Billing (balance + ledger-backed topup + history). Session
> claims drive the sidebar filter (`advertiserNav` + `filterNav`) and tenant scope (advertiser
> sessions filter report queries by `account_id`). Enabling fixes: gateway now injects its
> service API key on proxied campaign/placement/creative calls (`gateway.service_api_key`,
> dev default = seeded key), campaigns/reports got exact+subtree registrations (no more 307s,
> `PATCH /v1/api/campaigns/{id}` works through the gateway), method-aware permission gate
> (`RequirePermissionByMethod`). Remaining: creatives/audiences/settings screens, campaign
> drill-down (IO › line item), pacing viz, wizard.
- **Persona / gate:** `advertiser:{owner,manager,analyst,finance,viewer}` — `campaigns:*`,
  `creatives:*`, `audiences:*`, `reports:*`, `billing:*`.
- **Job-to-be-done:** "Launch and optimise campaigns, watch spend/performance, manage creatives &
  audiences, handle billing."
- **Screens:** Dashboard (spend/pacing/perf KPIs) · Campaigns (IO › line-item › creative;
  create/edit/pause via existing DSP CRUD) · Creative library (**GAP: upload API**) · Audiences
  (upload exists; management **GAP**) · Reports (query API exists; **save/schedule GAP**) ·
  Billing & balance (`/v1/billing/summary|ledger`; **topup GAP**) · Team (**GAP**) · Settings ·
  Onboarding wizard (**GAP**).
- **APIs:** exists — `APICampaigns`, `APIReports`, `APIAudiences` (upload), `BillingSummary/Ledger`,
  `AdCreatives` (list/bandit). GAP — creative upload, audience CRUD, saved/scheduled reports,
  team/user mgmt, topup, onboarding.
- **Components:** stat cards, data-table (campaigns), chart (spend/perf), wizard (create campaign),
  modal (creative upload), pill filters, badge (status).
- **Data:** insertion_orders, line_items, creatives, targeting_rules, audience_segments, invoices,
  advertiser_balances, saved_reports.
- **Prompts:** campaign hierarchy in one screen or drill-down? pacing viz? creative preview render?
  what's the "first campaign in 5 minutes" wizard?

### Publisher portal  ⬜
- **Persona / gate:** `publisher:{owner,manager,ad_ops,analyst,finance,viewer}` — `placements:*`,
  `deals:*`, `quality:*`, `earnings:view`, `pipeline:*`.
- **Job-to-be-done:** "Manage inventory & floors, cut deals, watch fill & earnings, get my ad tag."
- **Screens:** Dashboard (fill rate, eCPM, earnings) · Inventory/Placements (SSP CRUD exists) ·
  Deals (**GAP: deals CRUD API**) · Quality controls (**GAP**) · Earnings/Payouts (**GAP**) · Ad
  tag / integration (snippet gen) · Reports (query API) · Team · Onboarding wizard (**GAP**).
- **APIs:** exists — `APIPlacements`, `SSPPublishers`, `APIReports`, `BillingSummary` (rev side),
  `DebugBillingRates` (revshare). GAP — deals CRUD, quality controls, payouts/earnings statements,
  ad-tag endpoint, onboarding.
- **Components:** stat cards, data-table (placements/deals), chart (fill/earnings), snippet/copy
  block (ad tag), badge, drawer (placement detail).
- **Data:** publishers, placements, deals, quality_controls, publisher_line_items, payouts.
- **Prompts:** floor-price editing UX (inline)? ad-tag formats (JS/Prebid/VAST)? payout statement
  shape?

### Agency portal  ⬜
- **Persona / gate:** `agency:*` + `ManagedAccounts[]`.
- **Job-to-be-done:** "Switch between the advertiser/publisher accounts I manage; portfolio view."
- **Screens:** Account switcher (act-as) · Portfolio dashboard (roll-up across managed accounts) ·
  then reuse advertiser/publisher screens scoped to the selected account.
- **APIs:** reuses advertiser/publisher APIs with the switched `AccountID`; **GAP: managed-accounts
  listing + agency↔account mapping API** (today it's only in the JWT claim).
- **Prompts:** how is `ManagedAccounts` populated/managed? cross-account roll-up metrics? per-client
  permissions?

### Staff / Account-manager console  ⬜
- **Persona / gate:** `staff:owner` — `moderation:*`, `fraud:*`, `support:*`, `config:*`, `ops:*`,
  `audit:read`. (Superuser via `CanAccessAccount`.)
- **Job-to-be-done:** "Review creatives, tune fraud rules, manage config/secrets, support accounts,
  read the audit trail."
- **Screens:** Moderation queue (creative `review_status`; **GAP: moderation API**) · Fraud rules
  (`fraud_blocklists`; **GAP: CRUD API** — data exists, tracker consumes it) · **Config manager
  (EXISTS — /dev/console)** · **Secrets (EXISTS)** · Audit log (`audit_log` read; a viewer screen)
  · Accounts admin (**GAP: account CRUD**) · Support / impersonation.
- **APIs:** exists — config, secrets, `DebugExchangeRouting`, warm-cache dumps. GAP — moderation,
  fraud CRUD, accounts admin, audit-log query API.
- **Prompts:** moderation SLA/queue design? which config graduates from "dev console" to a
  supported staff tool? impersonation safety + audit?

### Admin  ⬜
- **Persona / gate:** `admin:owner` (`*`).
- **Job-to-be-done:** "Everything staff can do + platform ops (deploys, A/B, canary, full config)."
- **Screens:** all of the above + ops (A/B tests, canary, deployment ledger), platform-wide health.
- **Prompts:** what's admin-only vs staff? reuse the operator console's Services/Pods tabs.

---

## API Gaps register (backend the UI needs — each is a "prompt")

The UI can't ship a screen without its API. These are the net-new endpoints, ordered by how many
portals they unblock. Each becomes its own small design when picked up.

| Gap | Unblocks | Sketch |
|---|---|---|
| **Real auth/login + session** | all portals | ✅ SHIPPED (F4a) — login → bcrypt verify → JWT → cookie; logout. Remaining: refresh, agency act-as, password reset. |
| **Signup + accounts + team CRUD** | all | ✅ SHIPPED — `POST /v1/auth/signup` (account+owner), `GET/POST /v1/api/team` (list/invite, tenant-scoped). Remaining: edit/remove member, agency mappings. |
| **Creative upload** | advertiser | ✅ SHIPPED — `POST /v1/api/creatives` → `creatives` row, review_status=pending_review (→ moderation queue). Remaining: asset (image) upload to S3/Minio, edit/delete. |
| **Deals CRUD** | publisher, advertiser | ✅ SHIPPED (create/list) — `GET/POST /v1/api/deals`, publisher-owned, invalidates the exchange cache. Remaining: edit/pause, allowlists, PG volume. |
| **Saved + scheduled reports** | advertiser, publisher | ✅ SHIPPED (create/list/delete) — `GET/POST/DELETE /v1/api/reports/saved`, persists `saved_reports` with optional cron schedule + delivery. Remaining: a runner that fires the schedule → email/export. |
| **Moderation queue API** | staff | ✅ SHIPPED — `GET/POST /v1/api/moderation`, list pending creatives + approve/reject (reason required to reject), platform-wide. Remaining: appeal flow, bulk actions. |
| **Fraud rules CRUD** | staff | ✅ SHIPPED — `GET/POST/DELETE /v1/api/fraud/blocklists`, manages `fraud_blocklists` (ip/ua/domain/app_bundle), invalidates the tracker warm cache. Remaining: rule expiry, ads.txt overrides. |
| **Publisher payouts/earnings** | publisher | ✅ SHIPPED (read) — `GET /v1/api/payouts`, tenant-scoped history + pending/paid rollup on top of `payouts`. Remaining: downloadable statements, reconciliation vs ledger. |
| **Webhooks CRUD** | advertiser/publisher | ✅ SHIPPED (create/list/delete) — `GET/POST/DELETE /v1/api/webhooks`, per-account subs with a once-shown HMAC secret, invalidates the dispatcher cache. Remaining: pause/edit, delivery-log view, retries UI. |
| **Quality controls CRUD** | publisher | ✅ SHIPPED — `GET/POST/DELETE /v1/api/quality-controls`, one row per (publisher, type) across the 5 blocklist/allowlist types, publisher-ownership checked. Remaining: exchange/adserver consumption + a cache-invalidate subject. |
| **Ad-tag generator** | publisher | ✅ SHIPPED — `GET /v1/api/adtag?placement_id=&tag_type=js\|prebid\|vast`, derives a paste-ready snippet from the placement + pubad routes. Remaining: signed/tokenized tags, size-list from creatives, copy-box UI. |
| **Topup / billing actions** | advertiser | ✅ SHIPPED (dev payment) — `GET/POST /v1/api/billing/topup` (billing:view / billing:topup). Idempotency-keyed (`UNIQUE(account_id, idempotency_key)`, replay → 200 + duplicate flag, key reuse w/ different amount → 409); one tx writes the topup row + double-entry `ledger_entries` pair (debit `platform:cash` / credit `advertiser:{id}:balance`) + `advertiser_balances` upsert, so balance stays ledger-derivable. Migration 029 (+RLS). Payment approval is the fake instant-success "dev" method. Remaining: real payment provider (replaces only the approval step → pending/webhook flow), TigerBeetle mirror once topups meet spend reconciliation, topup UI. |

---

## Delivery roadmap (phased, ratcheting)

Each phase ships end-to-end (auth-gated screens on real APIs, green tests) before the next.

- **Phase F — Foundation** (theme tokens → app shell → component set for Phase 1 → real auth →
  guardrails). *Nothing customer-facing ships without F4.*
- **Phase 1 — Advertiser MVP:** Dashboard + Campaigns (CRUD exists) + Reports (query exists) +
  Billing view. Proves the shell + components + auth on real APIs. **API adds:** login/session,
  accounts/team read.
- **Phase 2 — Publisher MVP:** Dashboard + Placements (CRUD exists) + Earnings (view) + ad tag.
  **API adds:** payouts/earnings read, ad-tag endpoint.
- **Phase 3 — Staff console:** Moderation + Fraud rules + Audit viewer; graduate the operator
  console (Config/Secrets already exist) into the staff shell. **API adds:** moderation, fraud CRUD.
- **Phase 4 — Depth & the rest:** creative upload, deals CRUD, saved/scheduled reports, audience
  mgmt, onboarding wizards, agency multi-account, webhooks, admin ops.

**Sequencing logic:** advertiser first (its CRUD + reporting APIs already exist → fastest path to a
real portal that validates F). Publisher second (placement CRUD exists; earnings needs a little
backend). Staff third (config/secrets exist; moderation/fraud need APIs). Everything with a big
API gap lands in Phase 4.

---

## Cross-cutting "what we need to support" prompts (expand anytime)

- **Auth/session:** cookie model, refresh, password reset (Mailpit), MFA, agency act-as, API-key
  self-service (per-user keys, `api_keys.scoped_permissions`).
- **Permissions → UI:** central `nav = filter(allNav, HasPermission)`; disable vs hide; 403 pages.
- **Multi-tenancy:** every data call carries the account context; agency switch; staff impersonation
  (audited).
- **Real-time:** which numbers must be live (spend, budget pacing, fill) → HTMX poll vs SSE vs
  manual refresh; reuse the warm-cache/`/debug/*` freshness model.
- **i18n / currency / timezone:** accounts carry currency; line_items carry timezone — surface both.
- **Accessibility:** keyboard nav, focus states, ARIA on components, contrast in both themes.
- **Empty/first-run:** every screen needs an empty state + a "do the first thing" CTA.
- **Errors:** toast + undo, inline field errors, never a blocking dialog.
- **Observability:** keep the operator/console deep-links (Grafana/Jaeger) available to staff/admin.
- **Testing:** handler tests (permission gate → 200/403), template render smoke tests, HTMX fragment
  tests; e2e for the golden path per portal.

---

## Immediate next actions (ready to execute)

1. **F1 tokens** — consolidate `theme.css` into the single token set (light+dark, service palette),
   map Tailwind config to it in the head partial. *(additive, low-risk)*
2. **F3 components** — create the Phase-1 component partials (`table`, `card`, `stat`, `form-field`,
   `badge`, `tabs`, `pill`, `empty-state`) as new files with a demo page. *(additive, zero-risk to
   existing pages)*
3. **F2 shell** — wire a real `layout.html` + role-aware nav; migrate the dashboard to it first.
4. **F4 auth** — login/session + claims-in-templates (unblocks Phase 1).
Then start **Phase 1 (Advertiser MVP)**.

> Keep expanding the Area Cards and the API Gaps register as each screen is picked up — that's the
> "self-expandable" contract of this doc.
