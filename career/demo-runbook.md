# Demo Runbook — Showing the Platform in an Interview

Everything you need to give a reliable, impressive live demo. The good news:
**the demo is already built** — four one-click guided flows in the staff portal,
plus a real publisher webpage. Your job is to rehearse it and de-risk it.

> **The golden rule: record a backup video first.** A live demo *will* pick the
> worst moment to break (network, screen-share, a wedged pod). Record the full
> demo working once (see "Backup" below). If anything fails live, you narrate over
> the recording and lose nothing. The recording also doubles as the `<demo-url>`
> asset for your resume/write-up.

---

## 🎯 Cue card — keep THIS open while you present

*(The detail behind each step is in "The narrative → Full arc" below. This is the
glance version for the live run.)*

### Quick links (click these — no typing)

Assumes port-forwards up (`gateway 8080`, `publisher-adserver 8088`) + `make
demosite` running. If localhost fails, swap `localhost:8080` → `192.168.64.2:8080`.

| Open | Link |
|---|---|
| 🔑 Log in (admin@adtech.local / admin) | http://localhost:8080/login |
| ⭐ **Staff portal → Demos** (the 4 flows) | http://localhost:8080/portal/staff#demos |
| 🔎 Trace Explorer | http://localhost:8080/dev/trace-explorer |
| 🎛️ **Publisher Simulator — ALL formats (tabs + inspectors)** | http://localhost:8080/dev/publisher-simulator |
| 🌐 Demo publisher page (real external site) | http://localhost:9000 |
| 🎬 Video pre-roll (VAST instream) | http://localhost:9000/video |
| 🔊 Audio ad (instream) | http://localhost:9000/audio |
| 🧩 Native (in-feed) | http://localhost:9000/native |
| 🛒 Demo advertiser (Ford, conversions) | http://localhost:9200 |

> **Format showcase → use the Publisher Simulator** (one page, a tab per format
> with request/response inspectors). The demosite (`:9000`) is the "real external
> website" version — nicer for the display/video "wow," but one format per page.
| 📊 Advertiser portal | http://localhost:8080/portal/advertiser |
| 📰 Publisher portal | http://localhost:8080/portal/publisher |
| 📈 Grafana (live dashboards) | http://192.168.64.2:3000 |
| 🧭 Jaeger (traces) | http://192.168.64.2:16686 |

**Tab order to pre-open:** login → staff/#demos → demosite (`:9000`) → Trace
Explorer.

**Before you share your screen (two commands):**
1. Cluster up? If Rancher Desktop is stopped: `rdctl start` (wait ~1 min).
2. **Terminal A:** `make demo-forward` — bridges the host to the k3s cluster
   (leave it running the whole demo).
3. **Terminal B:** `make demo-setup` — seeds, warms video/SSAI, generates
   traffic, and prints a **GREEN/RED per format** so you know everything serves.
4. Log into the portal, click each of the 4 demos once to warm them, and have the
   backup video in a tab.

> On Rancher, the host reaches the cluster **only** through `make demo-forward`
> (kubectl port-forwards) — without it, `localhost:8080` etc. are dead. That's the
> #1 gotcha.

1. **SAY** the framing: *"I built a full ad platform solo — the theme is
   transparency, you can trace any ad from the bid to the dollar. Let me show you."*
2. **OPEN** `localhost:9000` → accept consent → **SAY** *"real publisher page, real
   ad SDK, real ads from a live auction."*
3. **CLICK** Portal → **Demos → Auction Trace → Run demo** → **WALK the 5 steps**
   (persona → request+trace_id → auction/winner → serve+impression → where the
   data went) → **CLICK the Trace Explorer link** → **SAY** *"every hop, zero
   slippage."*
4. **CLICK** **Demos → Onboarding & Expansion → Run demo** → **SAY** *"this is my
   a prior ad-tech employer domain — one email upload resolved across devices into an audience."*
   → walk before/after.
5. *(if time)* **CLICK** **Demos → Billing → Run demo** → **SAY** *"every
   impression accrues spend + revenue, exactly once — money in = money out."*
6. **SAY** the close: *"That's the whole loop, live on Kubernetes. Happy to go
   deep on any layer."*

**Business-impact bookend (optional but strong):** *before* step 1, open the
**Advertiser** + **Publisher** portals and note the baseline; *after* step 5,
refresh them → spend up, winning campaigns + impressions, publisher revenue up.
(Logins below; details in "Show the business impact".)

**Pub-sim note:** Prebid mode **bids** after `demo-setup` (it sets a live
`exchange.schain_enforcement=warn` row that overrides the strict env). If it still
`nobid`s right after setup, the exchange hasn't polled config yet (≤30s) — re-fire.
(That `nobid` is itself the schain anti-spoof mechanism — see the Security section.)

**If something breaks:** re-click Run demo (no-bid/in-flight self-heal), or cut to
the backup video and narrate. Never troubleshoot silently for >20s.

---

## What you have (inventory)

| Asset | How to reach it | What it shows |
|---|---|---|
| **Staff portal → Demos** | Portal → sign in → "Demos" section | 4 guided flows (below) |
| ↳ Onboarding & Expansion | "Run demo" button | one email → cross-device audience (**your domain**) |
| ↳ Auction Trace | "Run demo" button | one real request → auction → win → serve → impression |
| ↳ Billing | "Run demo" button | the money loop (spend accrues, exactly once) |
| ↳ Rollups | "Run demo" button | data pipeline hot→cold |
| **Trace Explorer** | `/dev/trace-explorer?trace_id=…` | full timeline of one request across services |
| **Demo publisher site** | `make demosite` → `localhost:9000` | **real ads rendering** on a real webpage |
| **Demo advertiser (Ford)** | `make demoadv` → `localhost:9200` | retargeting pixel + conversion postback |

**Login:** `admin@adtech.local` / `admin` (staff). Portal is at the gateway.

---

## Access (local stack)

The stack runs in the `adtech` namespace. Two ways to reach it:

- **LoadBalancer:** gateway at `http://192.168.64.2:8080` (IP may drift — check
  `kubectl get svc -n adtech gateway-lb`).
- **Port-forwards (recommended for the demo — the demosite defaults to localhost):**
  ```
  kubectl port-forward -n adtech svc/gateway 8080:8080 &
  kubectl port-forward -n adtech svc/publisher-adserver 8088:8088 &
  ```
  Then the portal is `http://localhost:8080` and `make demosite` "just works"
  (its defaults point at `localhost:8080` + `:8088`).

---

## The narrative

Pick the arc that fits the role. Lead with the flow that matches the job.

### Full arc (~8–10 min) — "trace an ad from bid to dollar"

**0. Framing (30s).** *"I built a full programmatic ad platform solo — SSP,
exchange, DSP, ad server, an identity/data-onboarding pipeline, and a billing
ledger — to understand the whole machine end to end. The theme is transparency:
you can trace any ad request from the bid all the way to the dollar. Let me show
you, live."*

**1. A real ad renders (the hook, ~1 min).** Open `localhost:9000` (demosite).
Accept the consent banner. Ads fill the slots.
> *"This is a real external publisher page — it loads the real ad SDK and calls
> the real ad server, cross-origin, just like a live site. These creatives came
> from an auction that ran a moment ago."*

**2. Follow ONE request end to end (the flex, ~3 min).** Portal → **Demos →
Auction Trace → Run demo.** Walk the 5 steps aloud:
> Persona (who's browsing + consent posture) → Request fired (here's the
> `trace_id` that tags everything from here on) → Auction (exchange fanned out to
> DSPs, first-price with deal priority + bid shading, this DSP won at $X CPM) →
> Serve + track (creative served, impression recorded — the billable event) →
> Where the data went (same trace_id now flows to hot rollups, the cold lake, and
> billing).

Then click the **Trace Explorer** link → *"Same request, every hop, with
latencies. This is the zero-data-slippage guarantee — I can account for every
event."*

**3. Your domain — data onboarding (~3 min).** Portal → **Demos → Onboarding &
Expansion → Run demo.**
> *"This is the part I worked on at a prior ad-tech employer. Watch a single uploaded email get
> resolved across devices into a cross-device audience."* Walk before/after: the
> segment starts with one member (the email); identity resolution adds the
> cookie + device id; the segment is now cross-device and ready to activate in an
> auction. Call out the privacy angle (consent-gated, min-aggregation, GDPR purge).

**4. The money (~1 min, if time).** Portal → **Demos → Billing → Run demo.**
> *"Every impression accrues advertiser spend and publisher revenue, exactly once.
> I can prove money-in = money-out to a cent over a soak test."*

**Close (30s).** *"That's the whole loop, running locally on Kubernetes. Happy to
go deep on any layer — the auction, the identity graph, the exactly-once
accounting, whatever's most relevant."*

### Short arc (~4–5 min)
Framing → demosite ad renders → **Auction Trace** demo → Trace Explorer → close.
(Drop onboarding/billing; offer them as "I can also show you…".)

### Data-onboarding-role arc
Reorder: Framing → **Onboarding & Expansion** demo (lead with it) → demosite ad
render → Auction Trace to show the *activation* side → close on privacy/ARA.

---

## Format showcase (the complex formats — your ad-tech specialty)

Do this on the **Publisher Simulator** (`/dev/publisher-simulator`) — one page, a
tab per format, each with a request/response inspector so you can show the real
OpenRTB round-trip. Walk the tabs in this order (simple → complex, ending on CTV):

1. **Display** *(baseline, ~20s)* — "Standard banner. Real auction behind it —
   SSP → exchange → DSP. This is the simple case; everything else builds on it."
2. **Native** *(~20s)* — "In-feed native: the DSP returns structured assets
   (title, image, CTA), the ad server renders them into the card. Same auction,
   richer response."
3. **Audio (DAAST)** *(~30s)* — "Podcast/streaming spot. The player fetches an
   audio VAST, plays the ad's media file, then the episode, firing quartile beacons
   through the tracker."
4. **Video (VAST / VMAP)** *(~1 min — the first complexity flex)* — pre-roll plays,
   quartile beacons fire. Then call out the hard parts:
   - **VMAP** = multiple ad breaks in one stream (pre/mid/post-roll).
   - **Ad pods** = several ads *within* one break, sequenced, with **competitive
     separation** (never two ads from the same advertiser in a pod). "Each break is
     its own per-slot auction — that's a lot more than a single banner."
5. **SSAI (CTV)** *(~2 min — THE headline)* — pick **"Video · HLS (CTV)"**, click
   **Play SSAI Stream**. Watch: a **pre-roll** ad plays first (badge = "Stitched Ad
   · server-side beacons"), then content, then **partway through a MID-ROLL ad
   plays inline** — same player, no separate ad call. Expand **"Stitched HLS
   manifest"** to show it: `#EXT-X-CUE-OUT` (pre-roll) → transcoded ad segments →
   `#EXT-X-CUE-IN` → content → `#EXT-X-DISCONTINUITY` + `#EXT-X-CUE-OUT` (mid-roll)
   → more transcoded ad segments. This is **server-side stitching *with*
   transcoding** — distinct from the demo sites' client-side VAST pre-roll.
   > *"This is server-side ad insertion — the CTV standard, and what I worked
   > around at a prior ad-tech employer. The ads — pre-roll AND mid-roll — are stitched into the
   > manifest server-side, so the ad segments are indistinguishable from content
   > (ad-blocker-proof). The transcoder conditions each ad onto the content's
   > bitrate ladder first so the splice is seamless, and the impression/quartile
   > beacons fire **server-side** as the player fetches each ad segment, not from
   > client JS. Notice the mid-roll is spliced *between content segments* with a
   > DISCONTINUITY marker — that's a real mid-roll break, not a pre-roll."*
   > **Note:** SSAI is **not** on the demo sites (`:9001-9500`) — those do
   > client-side VAST. The stitched stream is here in the Publisher Simulator only.
6. **The exotic auction types** *(~30s, optional breadth)* — flip through **DOOH**,
   **Retail**, **In-game** tabs: "Five distinct auction strategies — DOOH is one
   screen, many viewers (an impression multiplier + timeslot auction); Retail is
   sponsored-product relevance (a GSP-style auction); In-game is a batch auction.
   Same money loop underneath."

**What to emphasize (why these are *complex*):** VMAP/pods = sequencing +
competitive separation + per-break auctions; SSAI = manifest rewriting + ABR
conditioning + server-side beaconing; DASH/CMAF = multi-period; DOOH = the
one-to-many impression model. That breadth — and knowing *why* each is hard — is
the differentiator.

> **Warm-cache caveat (important):** SSAI conditions the winning ad on the first
> manifest fetch, so the first stream can take ~10–30s to fill. `make demo-setup`
> already warms it and prints `ssai GREEN` — never demo SSAI cold; run demo-setup
> first and confirm GREEN. **Known gotcha (verified 2026-09-27):** if SSAI shows no
> ad, the usual cause is a polluted video creative winning the break — a leftover
> `.test`-URL test artifact (`vvast`) whose asset can't be fetched, so it never
> conditions. **`make reset` clears it** (wipes + reseeds clean) — that's the fix
> we proved. Diagnose via `kubectl logs -n adtech -l app=transcoder | grep "condition failed"`.

> **Make the ad LOOK different from the content (for a visible transition):** the
> SSAI content is Big Buck Bunny, and many seeded ad creatives are *also* Big Buck
> Bunny — so a stitched ad can be invisible (bunny-into-bunny). `demo-setup [2c]`
> rejects the `bbb` video ad creatives so a **Sintel** ad (a different film) wins the
> break → a clear **bunny content → Sintel ad → bunny content** cut, with the
> "Stitched Ad" badge lit during the ad. **Reliability:** conditioning is async per
> session, so the *first* Play can no-fill / take 2–3 tries — **click "Play SSAI
> Stream" once in pre-flight to warm it.** For a technical audience the **Network tab
> + stitched manifest** (content `bbb-*`/`seg_N.ts` vs the break's `seg?ad=…` →
> server-side beacon 302 → transcoded ad segment) is the clearest proof of SSAI.

---

## Show the business impact — portals before & after (do this!)

Numbers moving in the **real product UI** is the most convincing part of the whole
demo. Do a **before → traffic → after** on the customer portals so the audience
sees the campaigns that won and the money change on both sides.

**Logins** (all password `admin`):
- **Advertiser:** `advertiser@adtech.local` (or `adv-acme@adtech.local`)
- **Publisher:** `publisher@adtech.local` (or `tech-review@adtech.local`)
- **Staff** (can *Impersonate* any account): `admin@adtech.local`

**Flow:**
1. **Before** — open the **Advertiser portal** (campaigns, spend, impressions,
   budget pacing) and the **Publisher portal** (fill rate, revenue/earnings). Note
   the baseline — or `make reset` first for a clean zero.
2. **Generate** — run `make traffic` (or fire the Auction Trace demo a few times /
   click through the demosites) to push real auctions through.
3. **After** — refresh both portals and show the deltas: advertiser **spend up**,
   **which campaigns won** (+ impressions/clicks), and publisher **revenue + fill
   up**. Same auctions, both sides of the marketplace.

> Payoff line: *"every one of those auctions moved real money — here's the
> advertiser being charged and the publisher being paid, reconciled to the cent."*

Links: Advertiser `…/portal/advertiser` · Publisher `…/portal/publisher` · Staff
`…/portal/staff` (all on `localhost:8080`).

---

## Security & anti-spoof — the integrity story (strong demo)

The platform's edge is **transparency + integrity**: every externally-reachable
call path is protected against spoofing, and there's a **one-command harness** that
proves it live. This is a differentiated, senior-level thing to show.

**The model:** a secret can't live in a browser (devtools exposes it), so
browser-fired calls use **server-issued signed URLs + supply-chain auth + fraud
checks**; only server-to-server calls carry a real shared secret/signature.

### Call-path anti-spoofing (the headline)
| Mechanism | Prevents | How to show |
|---|---|---|
| **HMAC-signed tracker pixels** (imp/click/view, `sig`) | forged / replayed impressions & clicks | unsigned `/v1/t/imp` → **403** |
| **Per-advertiser conversion HMAC key** (validated by `advid`) | one advertiser forging a CPA conversion billed to **another** | conversion signed with the wrong key → rejected |
| **Trusted endpoint-bound seat** (data-fee) | a bidder dodging/misdirecting data fees via a fake `SeatBid.Seat` | fee always bills the seat bound to the **winning endpoint**, never the bidder's claim |
| **ads.txt / sellers.json** | domain spoofing / unauthorized resellers | `spoofer.example` (excludes us) → **nobid** |
| **schain (SupplyChain)** | opaque / forged supply paths | missing schain → **nobid** — *this is the `nobid` you saw in Prebid mode* |
| **ads.cert (Ed25519)** | forged / replayed bid requests to DSPs | unsigned request → **nobid** (same request bids with adcert off) |
| **JWT + `X-API-Key`** on `/v1/api/*` | unauthenticated management calls | no/invalid token → **401** |

### Platform defense-in-depth (mention; don't have to click each)
- **RBAC** — `resource:action` perms baked into the JWT; wrong role → **403**.
- **Multi-tenant RLS** — Postgres row-level security; an unscoped/cross-tenant query returns **0 rows** (fail-safe), enforced by the `adtech_app NOBYPASSRLS` role.
- **StripClientIdentityHeaders** — the edge strips client-supplied identity headers so a caller can't forge `X-Account-ID` through a CORS pass-through proxy.
- **Consent / privacy** — personalisation gated on `privacy.Evaluate()`; consent flows the whole chain; `public` vs `dsp_private` audience visibility.
- **Idempotency / replay** — `Nats-Msg-Id` + Redis dedup + PK-claim on every money write → exactly-once.
- **Also:** append-only **audit log** (UPDATE/DELETE revoked), per-IP **rate limiting**, **AES-256-GCM** secrets at rest, parameterized queries only.

### Demo it live — the enforcement harness
```
make security-harness ARG=on      # set up keys/ads.txt/schain, flip all 4 controls to STRICT
make security-harness ARG=status  # show what's enforced
make security-harness ARG=off     # revert to dev defaults (do this after)
```
With strict on (prerequisites set so legit traffic still fills ~77%), show **both directions**:

| Mechanism | Accept legit | Reject spoofed |
|---|---|---|
| ads.cert | exchange-signed request → bids | unsigned → nobid |
| ads.txt | authorised domain → bids | `spoofer.example` → nobid |
| schain | valid schain → bids | missing schain → nobid |
| tracker HMAC | signed pixel → recorded | unsigned pixel → **403** |

**Talking track:** *"Every spoofable call path is authenticated — browser pixels are
HMAC-signed and server-issued; conversions are signed per-advertiser so nobody can
forge a competitor's CPA; data fees bill the trusted endpoint-bound seat, not the
bidder's self-declared one; and the supply path is validated via ads.txt / sellers.json
/ schain. I can flip it all to strict and watch it reject spoofed traffic while legit
still fills."*

**Be honest about the gaps too (senior signal):** `docs/SECURITY.md` → "Known open
gaps" lists what's *not* covered and why (unsigned retargeting pixel, JWTs not
revocable before 12h, per-pod rate limiting assumes a CDN/WAF, etc.). Naming your own
gaps is a strong interview move.

> Note: `security-harness ARG=on` sets schain **strict** — that's exactly when Prebid
> mode (no schain) will `nobid`, *by design*. For the normal demo, `demo-setup` keeps
> schain at its default so Prebid mode bids. Run `ARG=off` when you're done showing security.

---

## Pre-flight checklist (do this ~30 min before)

1. **Cluster up.** `kubectl get pods -n adtech` — all `Running`? If Rancher Desktop
   is stopped: `rdctl start` (wait ~1 min). If it looks wedged after sleep:
   `make stack-doctor`. (Host uptime of weeks can cause a cascade only a **macOS
   reboot** fixes — so reboot the night before if it's been up for weeks.)
2. **Bridge the host to the cluster.** `make demo-forward` in its own terminal —
   this is mandatory on Rancher (the host can't reach the services otherwise).
   Leave it running.
3. **One-command warm + verify.** `make demo-setup` in a second terminal. It seeds,
   refreshes caches, conditions video/SSAI (`prewarm`), generates baseline traffic,
   and prints **GREEN/RED per format** — so you *know* display/native/video/audio/
   SSAI all serve before you rely on them. Re-run if a format is RED. Do **not**
   run `make reset` right before — it wipes everything.
4. **Warm the guided demos.** Click **Run demo** on each of the four *once* — they
   cache the last run and flush any first-run flakiness. If Auction Trace no-bids,
   just re-run (the persona is chosen to win; it retries 3×).
4. **Port-forwards up** (gateway:8080, publisher-adserver:8088) and
   **`make demosite` running**; confirm ads render at `localhost:9000`.
5. **Tabs pre-opened, logged in:** portal Demos section, Trace Explorer,
   `localhost:9000`. Zoom the browser to ~125–150% for screen-share legibility.
6. **Optional but great:** `make traffic` running quietly in the background so any
   dashboard you show is live, not static.
7. **Backup video open in a tab**, ready to play if live fails.

---

## Failure modes & instant recovery

| Symptom | Fix (say it out loud, stay calm) |
|---|---|
| Auction Trace shows "no-bid" | Just click **Run demo** again — "fill can be flaky; the persona's picked to win." It auto-retries 3×. |
| "events still in flight" | Re-run; the pipeline is async (that's *honest* — mention it's real, not staged). |
| Ads don't render on demosite | Check port-forwards are up; hard-refresh; accept the consent banner. |
| Portal won't load | Use the LB IP `http://192.168.64.2:8080` instead of localhost. |
| Pods crash-looping / connection refused | `make stack-doctor`; if no dice, switch to the **backup recording**. |
| Total stack failure | Play the recording, narrate over it. You lose nothing. |

**Never** troubleshoot silently on a shared screen for more than ~20s — narrate,
or cut to the recording. Interviewers judge composure more than uptime.

---

## Backup recording (do this — it's the safety net)

Record a clean run of the **Full arc** with narration (QuickTime screen recording
on macOS, or Loom). ~8 minutes. This gives you:
- A fallback if the live demo breaks.
- A shareable `<demo-url>` for your resume/portfolio write-up (no hosting cost).
- A rehearsal artifact — watch it, cut the hedging, tighten the story.

Store the link in `career/portfolio-writeup.md` (the `<demo-url>` placeholder).

---

## Likely questions mid-demo (have crisp answers)

- *"Is this real or mocked?"* → "The serving, auction, tracking, identity, and
  money paths are real against live Postgres/ClickHouse/Redis/NATS/S3. Traffic is
  simulated but goes through the *real* serve path — real bid requests, real
  auctions." (Honesty reads as credibility.)
- *"How does the trace_id work?"* → minted at the SSP, propagated via header +
  gRPC metadata + NATS message, stamped on every log/row. (Module 2.)
- *"How do you know money isn't lost?"* → single AuctionWinEvent source of truth +
  reserve/settle + a VERIFY invariant, ±$0.01 over soaks. (Module 3.)
- *"How does the identity resolution work?"* → deterministic + probabilistic edges,
  union-find clustering into persons/households. (Module 4.)
- *"What's the scale?"* → ~180 rps lossless on 8 vCPU; here's how I'd shard to
  millions. (Module 9 — own the gap.)

> Deep-dive answers live in `career/learning-path.md`. Run Module 11 mock
> interviews before the real thing.

---

## One-line reminders

- Rehearse the **spoken narrative**, not just the clicks — the story is the point.
- Lead with the flow that matches the role.
- Record the backup. Record the backup. Record the backup.
