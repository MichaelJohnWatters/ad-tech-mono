# Portal UX backlog — audiences & campaign management

Born from the 2026-10-04/05 demo-video prep: a real user driving the advertiser
portal end-to-end (CRM upload → targeting → live bidding) hit every rough edge
in this area, and each item below maps to a confusion or bug we actually
observed. Already shipped from that session: audience checkbox pickers in the
campaign forms + the upload modal's append-to-existing selector (`61f0c76`),
the dead component-`onclick` fix (`5afc03c`).

Ordering = recommended order of attack. Effort: S (<½ day) / M (day-ish) /
L (multi-day).

## 1. Background writers must not rewrite segment metadata — **BUG, do first** (S)

`pkg/audience/store/postgres` `UpsertSegment` ON CONFLICT overwrites
`visibility`/`type`/`name` with the caller's values; the hourly
identity-expansion writer upserts with its own defaults and silently flipped a
`dsp_private` CRM list to `public` overnight (observed on the Lumière demo
segment). Privacy-adjacent. Fix: members-only add path for background writers
(enrollment should never touch segment metadata), or preserve metadata in the
conflict clause unless the writer is the owning portal/API.
**Done when:** upload `dsp_private` → run the conductor → still `dsp_private`
(e2e asserts it). Also in the PLAN.md ledger.

## 2. "Targeted by" column on the Audiences list (S)

Show which campaigns include each audience (reverse lookup over
`targeting_rules.include_segments`). "This audience is targeted by: nothing"
would have instantly explained why an upload produced no bids (the
"Diamond 2" hour). Likely a small gateway join on the audiences list response
+ one column in the table.

## 3. Warn on upload to an UNWIRED audience (S — pairs with #2)

Upload success toast (and/or a row badge): "no campaign targets this audience
— members won't affect bidding, attach it under Campaigns → Targeting."
Kills the silent-no-op confusion class completely.

## 4. Campaign edit as its own page, not a modal (M/L)

The edit surface now holds budgets/flight/bid model + 17 targeting fields +
two audience pickers + bid modifiers + frequency caps + the creative picker —
far past modal scale, and modal state is fragile (lost on stray clicks, no
deep link, no browser back). Move it to a full-width section routed by hash
(`#campaign/<id>` — the portal's section router in `advertiser.html` already
switches on `location.hash`), with grouped panels (Budget & flight / Targeting
/ Audiences / Creatives / Delivery). The create modal can stay thin (name +
budget + bid) and land on the edit page after create. Deep-linkable campaign
URLs also make demos and support links better.

## 5. Serving-sync indicator per audience (M)

We fought invisible cache lag twice (Redis changelog drain, DSP in-process
preload). Surface it: "in serving cache: 10/10 · DSP snapshot 8s old" per
audience — Postgres member count vs Redis set size vs the DSP's preload age
(expose the latter via a tiny debug read). Fits the platform's
trace-everything thesis; turns "why isn't it bidding YET" into a glance.

## 6. Case-insensitive duplicate-name guard on upload (S)

The server happily created `diamond intenders` beside
`Diamond & Jewelry Intenders`. On near-match (case/whitespace), return 409
with the existing audience's id/name; UI offers "add to that instead". The
dropdown solves the portal path — this covers raw API callers.

## 7. "Test a hash" box on the audience detail (S)

Paste an email → hashed client-side (never sent raw) → answers: "is this hash
a member? does the identity graph know it?". Doubles as the live demo prop
for the CRM story and a real onboarding debugging tool.

## 8. Member sample + source view (S)

First N member hashes with their `source` (`api`, `identity-expansion`,
`retargeting`) and `added_at`. The overnight +5 mystery members were only
diagnosable via psql.

## 9. Unmatched-rows detail on uploads (M)

Match rate says "10%"; show the split (matched / never-seen-by-platform) and
per-row status on the ingest job detail — the industry-standard "match report"
a real CRM onboarding team expects.

## 10. Promote the chromedp smokes into `make test-portal` (M)

Login → upload-append → picker round-trip → **a dead-onclick regression test**
(the `html/template` JS-escaping bug shipped silently and was only caught by a
human clicking). Two throwaway scripts were written in two days; keep one
suite under `cmd/portalsmoke` or `tests/browser`, same chromedp pattern as
`cmd/advertisersmoke`.

## 11. `dsp.identity_resolution_enabled=true` by default in local values (S)

The CRM demo dies without it, the preload resolver was explicitly built
QPS-safe, and off-by-default in dev manufactures mystery no-bids. Flip the
seeded default for dsp-internal in local/dev values only.
