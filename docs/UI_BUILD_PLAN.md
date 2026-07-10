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

> **Dev-bypass caveat:** the bypass identity is `AccountID: "dev-account"` — not a UUID, not
> an accounts row. Tenant-scoped gateway handlers that cast it (`$1::uuid`) must guard and
> serve an empty view (done: topup, payouts) or they 500 in dev. Sweep the remaining handlers
> (deals/team/saved-reports/webhooks/quality-controls) or switch the bypass to a seeded dev
> account UUID when picking up F4.

> **Real-auth mode (F4c, shipped):** the seed now installs an active `jwt_signing` secret
> (`dev-jwt-signing-key-…`) plus dev customer logins — `advertiser@adtech.local` (owner on
> adv-globex) and `publisher@adtech.local` (owner on pub-daily-news), password `admin`, joining
> the admin login. With the key present the gateway validates sessions for real: no more dev
> bypass, portal writes work in dev, unauthenticated API calls 401. The gateway reads the key
> at BOOT — restart it (`tilt trigger gateway`) after the first seed. e2e: `harness.LoginAs` /
> `CreateLoginUser` give tests real tenant sessions; the topup suite now exercises the full
> HTTP tenant flow (login → credit → idempotent replay → 409 on key reuse → ledger pair).
> NOTE: harness Reset truncates team_members — reseed (or CreateLoginUser) after a reset.

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
> (`RequirePermissionByMethod`). Phase 4 additions: Creatives screen (library list with review state +
> upload modal → moderation queue; gateway creative handler grew a tenant-scoped GET) and
> Saved queries (save/run/delete the report console's query via `/v1/api/reports/saved`).
> Remaining: audiences/settings screens, campaign drill-down (IO › line item), pacing viz,
> wizard, scheduled-report runner.
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

### Publisher portal  🟡 (MVP live)
> Shipped (Phase 2 MVP): `/portal/publisher` (alias `/dev/portal/publisher`) — Dashboard
> (ad requests / impressions / fill rate / gross earnings + 7-day trend from the reports
> query, publisher-scoped), Placements (list/create/pause/edit-floor via `/v1/api/placements`
> → SSP CRUD, exact+subtree registration with method-aware perms), Ad tag (placement picker →
> `/v1/api/adtag` js/prebid/vast + copy button), Earnings (`/v1/api/payouts` pending/paid +
> history). New `/v1/api/publishers` proxy = tenant-scoped "my sites" source (top-bar site
> switcher when an account has several). Shares `portalHandler` with the advertiser portal
> (claims → filtered nav + tenant scope). Tenant enforcement is server-side: SSP filters
> reads by the forwarded identity; the reports proxy verifies/injects `publisher_id`.
> Phase 4 additions: deals pause/resume + edit (new `PATCH /v1/api/deals/{id}`,
> publisher-owned, exchange cache invalidate) and Saved queries in the reports console.
> Remaining: fill-rate per placement, payout statements, net (post-rev-share) earnings.
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

### Agency portal  ✅ (switcher + portfolio roll-up shipped)
- **Persona / gate:** `agency:*` + `ManagedAccounts[]`.
- **Job-to-be-done:** "Switch between the advertiser/publisher accounts I manage; portfolio view."
- **Screens:** Account switcher (act-as) · Portfolio dashboard (roll-up across managed accounts) ·
  then reuse advertiser/publisher screens scoped to the selected account.
- ✅ **Act-as mechanism + managed-accounts API shipped** (migration 032, agency roles, login loads
  ManagedAccounts, proxy validates X-Act-As-Account against the managed set + forwards the advertiser
  tenant, staff `/v1/api/agency-accounts` assign/list/unassign — verified live). ✅ **Portal
  SHIPPED**: agencies reuse the advertiser portal (portalHome → advertiser) with an "Acting as"
  switcher (top bar, `.IsAgency` only) that sets an `act_as_account` cookie the proxy honours on
  every proxied call — all screens scope to the chosen client. ✅ Portfolio roll-up shipped (agency-only
  #portfolio section: per-managed-account spend/impressions/clicks + totals via per-account act-as report queries). Remaining: none core
  (cross-account dashboard).
- **APIs:** reuses advertiser/publisher APIs with the switched `AccountID` via the X-Act-As-Account
  header (API clients) or the act_as_account cookie (portal switcher).
- **Prompts:** how is `ManagedAccounts` populated/managed? cross-account roll-up metrics? per-client
  permissions?

### Staff / Account-manager console  🟡 (MVP live)
> Shipped (Phase 3 MVP): `/portal/staff` (alias `/dev/portal/staff`) — Moderation queue
> (pending creatives platform-wide, approve / reject-with-reason via `/v1/api/moderation`),
> Fraud rules (add/unblock the 4 blocklist types via `/v1/api/fraud/blocklists`, invalidates
> the tracker cache), Audit log viewer on the NEW `GET /v1/api/audit` (audit:read, platform-
> wide, exact-match filters action/resource_type/resource_id/account_id, limit≤500), and a
> Tools section linking the operator surfaces (config manager/secrets, pub sim, trace
> explorer, Grafana, Jaeger). Nav is permission-filtered — a moderation-only role sees just
> its queue. Remaining: accounts admin, support/impersonation, moderation appeal flow,
> graduating config manager into the shell proper.
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
| **Creative upload** | advertiser | ✅ SHIPPED — `GET/POST /v1/api/creatives` (tenant-scoped library list with review state + upload → pending_review → moderation queue). Remaining: asset (image) upload to S3/Minio, edit/delete. |
| **Deals CRUD** | publisher, advertiser | ✅ SHIPPED (create/list/edit/pause + depth) — `GET/POST /v1/api/deals` + `PATCH /v1/api/deals/{id}` (name/price/status **+ advertiser/placement allowlists, flight dates, PG guaranteed_volume**; placement allowlist ownership-checked; empty allowlist = match-all, which the exchange matcher already consumes; cache-invalidating). ✅ Portal placement allowlist is a checkbox multiselect (was hand-typed UUIDs). Remaining: none core. |
| **Saved + scheduled reports** | advertiser, publisher | ✅ SHIPPED (create/list/delete) — `GET/POST/DELETE /v1/api/reports/saved`, persists `saved_reports` with optional schedule + delivery. Portal UI shipped (save/run/delete in both report consoles). ✅ **Runner SHIPPED** — `cmd/report-runner` (pkg/reportrunner) fires interval schedules (@hourly/@daily/@weekly/@monthly) → emails the owner (migration 033 last_run_at; Tilt resource; verified live). Remaining: webhook delivery + full cron syntax. |
| **Moderation queue API** | staff | ✅ SHIPPED — `GET/POST /v1/api/moderation`, list pending creatives + approve/reject (reason required to reject), platform-wide. Remaining: appeal flow, bulk actions. |
| **Fraud rules CRUD** | staff | ✅ SHIPPED — `GET/POST/DELETE /v1/api/fraud/blocklists`, manages `fraud_blocklists` (ip/ua/domain/app_bundle), invalidates the tracker warm cache. Remaining: rule expiry, ads.txt overrides. |
| **Publisher payouts/earnings** | publisher | ✅ SHIPPED (read) — `GET /v1/api/payouts`, tenant-scoped history + pending/paid rollup on top of `payouts`. Remaining: downloadable statements, reconciliation vs ledger. |
| **Webhooks CRUD** | advertiser/publisher | ✅ SHIPPED (create/list/delete) — `GET/POST/DELETE /v1/api/webhooks`, per-account subs with a once-shown HMAC secret, invalidates the dispatcher cache. ✅ **Dispatcher SHIPPED** (`cmd/webhooks`, 2026-07-05) — consumes budget/balance/campaign events → HMAC-signed POST with retries + `webhook_deliveries` log. Remaining: pause/edit, delivery-log view API + UI. |
| **Quality controls CRUD** | publisher | ✅ SHIPPED — `GET/POST/DELETE /v1/api/quality-controls`, one row per (publisher, type) across the 5 blocklist/allowlist types, publisher-ownership checked. Remaining: exchange/adserver consumption + a cache-invalidate subject. |
| **Ad-tag generator** | publisher | ✅ SHIPPED — `GET /v1/api/adtag?placement_id=&tag_type=js\|prebid\|vast`, derives a paste-ready snippet from the placement + pubad routes. Remaining: signed/tokenized tags, size-list from creatives, copy-box UI. |
| **Topup / billing actions** | advertiser | ✅ SHIPPED (dev payment) + MONEY LOOP CLOSED — `GET/POST /v1/api/billing/topup` (billing:view / billing:topup), idempotency-keyed, ledger-honest (migration 029). The prepay balance now GATES bidding (DSP `BalanceGate`: fail-open Redis / fail-closed no-row) and DRAWS DOWN on realized spend (reporting billing `BalanceSink` writes a `spend` ledger pair + decrements balance, migration 030). Subjects `adtech.balance.depleted` + `adtech.cache.invalidate.advertiser-balances`. Verified live (topup→auction→impression→drawdown; exhaustion→no-bid→refill→resume). Remaining: real payment provider (pending/webhook flow), TigerBeetle mirror, topup UI already shipped in advertiser portal. |

---

## Depth backlog (recorded, not yet executed)

Self-serve *breadth* is done (signup → site/placement/campaign/creative→moderation/deal/topup,
and the money loop). What remains is *depth* — fields the seed/schema support but the create/edit
APIs + portal forms don't yet expose:

- **Campaign depth:** ✅ `bid_strategy`/`pacing_mode`/`total_budget`/flight-dates shipped;
  ✅ targeting geo/device/domain/category **include+exclude** shipped (create API + portal,
  gates bidding — verified). ✅ editing targeting via PATCH shipped (targeting_rules updated in the campaign PATCH tx +
  edit-drawer UI). ✅ OS / keyword (include+exclude) / inventory-type / segment
  (include+exclude) targeting shipped (create + PATCH API + portal + SSP stamps
  os/keywords/segments into the OpenRTB request; gates bidding — verified live).
  ✅ bid modifiers device/geo/time-of-day + campaign `timezone` shipped (create + PATCH API +
  portal; DSP applies before the floor check, time windows in the campaign tz — verified live).
  ✅ per-campaign frequency caps shipped (create + PATCH `frequency_cap {limit, window}` → ad-server
  warm cache over targeting_rules.frequency_caps; serve enforces advertiser cap, falls back to the
  platform default — verified live). ✅ creative attach + rotation shipped (campaign PATCH
  `creatives [{creative_id, weight}]` replaces line_item_creatives, approved+owned only;
  `creative_rotation` mode; advertiser portal edit-drawer creative picker — DSP serves the
  attached creative, verified live). ✅ Non-display campaign formats shipped: create accepts
  `format` (display|native|video|audio) → line_items.format; display auto-generates a placeholder
  banner, non-display starts creative-less (attach a real one via the creatives PATCH); portal
  Format select; models.Campaign exposes Format — verified live.
  Touch: `createCampaignRequest`/`patchCampaignRequest` in `cmd/dsp/management.go` + portal forms.
- **Deal depth:** ✅ allowlists + flight dates + PG `guaranteed_volume` shipped (API + portal).
  Video settings landed at the **placement** instead of `deal_config` (the SSP builds the video
  request from the placement, before any deal is matched — `deal_config` had no consumer): ✅
  placement `video_config` (skip/duration/mimes/protocols/plcmt, migration 034, applied by the SSP's
  buildVideoImp + publisher portal — verified live). ✅ Portal placement multiselect for deal allowlists
  shipped (checkbox picker, was hand-typed UUIDs).
- **Placement:** ✅ `floor_config` device/geo floors + ✅ time-based/dayparting floors shipped
  (`pkg/floors` resolver + SSP bid-path + API + portal dayparts textarea/timezone; gates bidding
  — both verified live). Remaining: (none — floor depth complete).
  **Publisher:** ✅ revshare editing shipped — flat fee + tiered/guaranteed/deal-type config +
  payment_terms (staff console `/v1/api/revshare`, validated + audited + billing-rates invalidate;
  writes the full revshare_config the ContractLoader decodes — verified live). ✅ **Direct-sold**
  `publisher_line_items` CRUD shipped (gateway `/v1/api/direct-line-items` + publisher portal
  "Direct sold" section; publishes the publisher-line-items invalidate — verified live).
- **Loose ends:** ✅ `adtech.cache.invalidate.ads-txt` now published by the adstxt crawler on a
  content change (crawler also wired into Tilt as a manual resource — verified live). IO
  management (campaign API auto-creates one IO/campaign today); `dsps` table stays operator-only.

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

## Status reconciliation (2026-07-09)

Phases F + 1–4 and the API Gaps register are effectively **shipped**: all three portals run on
real, tenant-scoped APIs (campaigns/placements/deals/moderation/fraud/payouts/billing/reports),
the money loop is closed, and campaign/deal/placement *depth* is done. Auth got its last two gaps
closed (logout button in the shell + constant-time JWT sig compare, 2026-07-09).

The remaining work is **depth, a few missing screens, and polish** — NOT backend wiring. The old
"Immediate next actions" (F1 tokens / F3 components / F2 shell / F4 auth) are all done and were
removed. What's actually left, ordered:

### Outstanding — cross-cutting (lifts all three portals)
- **App-shell consistency** 🟡 — portals are standalone full pages (own `<!DOCTYPE>`), not a shared
  `layout.html`; two nav systems coexist (`nav.html` for dev pages vs `app-sidebar.html` for
  portals). Consolidate onto one shell; show the logged-in user in the top bar.
- **States** — consistent empty/first-run (CTA), loading, and inline error states on every screen.
- **Visual polish** — density/hierarchy/contrast pass over the existing component set (needs a human
  in the loop to review the rendered result).

### Outstanding — Advertiser
- Campaign **drill-down** (IO › line-item › creative detail: config, targeting incl/excl, creatives,
  recent performance) — data exists (`line_items`/`targeting_rules`/`line_item_creatives`).
- **Pacing viz** (budget burndown vs flight) — reads existing spend/committed.
- **Audiences** management screen (upload API exists → needs list/view/edit) · **Settings** · **Team**
  screen (`/v1/api/team` exists) · onboarding **wizard** ("first campaign in 5 min").

### Outstanding — Publisher
- **Per-placement fill-rate** breakdown · **net (post-revshare) earnings** · downloadable **payout
  statements** · onboarding wizard. All read existing data/APIs.

### Outstanding — Staff
- **Accounts admin** (account CRUD — needs new backend) · **support / impersonation** (audited —
  needs new backend) · moderation **appeal/bulk** actions · graduate the **config manager** into the
  staff shell (exists at `/dev/console`).

> Keep expanding the Area Cards and the API Gaps register as each screen is picked up — that's the
> "self-expandable" contract of this doc.
