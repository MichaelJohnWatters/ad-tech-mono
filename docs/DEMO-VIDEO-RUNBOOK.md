# Demo video — run of show

The arc: cold start → the demo sites (ads + the behind-the-scenes panel) →
money/data appearing in the advertiser portal → the shop (retargeting + DPA +
conversion) → first-party data finale. Every scene lists the exact command/URL,
what appears on screen, and the one-line talking point.

## Pre-flight (before you hit record)

```sh
make stack-doctor                 # stack sane? (post-sleep wedges etc.)
make reset && make demo           # fresh world + seeded traffic (~3 min)
make traffic                      # LEAVE RUNNING in a background terminal —
                                  # keeps portal numbers ticking live on camera
```

- Browser: use a fresh profile / clear `*.adtech.local` cookies so every site
  shows its consent banner (the consent beat in scene 2 needs it).
- Logins (password `admin` for all): advertiser `adv-acme@adtech.local` ·
  publisher `tech-review@adtech.local` · staff `admin@adtech.local`.
- Dry-run scene 6's CSV upload once before recording — it's the only scene
  with a file-picker.
- Keep the host otherwise idle (no builds) — ad latency on camera stays crisp.

## Scene 1 — cold start (terminal, ~30s)

Show `make reset && make demo` output scrolling (or replay it: it ends with
"✔ Demo ready"). **Say:** "entire programmatic stack — SSP, exchange, DSPs, ad
servers, tracking, billing — on local Kubernetes; one command seeds 32
campaigns across 16 publishers and pushes real traffic through the real
serving path. No mock data anywhere."

## Scene 2 — the demo sites (~2 min)

All are REAL external publisher sites embedding the real SDK/VAST tags
(cross-origin, consent-gated):

| Site | URL | Shows |
|---|---|---|
| The Demo Times (news) | http://chronicle.adtech.local | display + native in-feed |
| ViewTube | http://viewtube.adtech.local | VAST video pre-roll |
| SoundWave | http://soundwave.adtech.local | audio ads |
| Twitchr (live) | http://twitchr.adtech.local | SSAI — ads stitched server-side into the live HLS manifest |
| Gadget / PrimeReel | http://gadget.adtech.local · http://primereel.adtech.local | more formats/layouts |

On chronicle: open the **🔍 Trace panel** (right edge) FIRST, then **Accept
personalized** on the consent banner. Scroll — ads fill, the panel logs every
call: ad request → **✓ FILL (advertiser · price)** → **👥 audience chip** (the
segments the SSP resolved and sent to the DSPs) → impression/viewability
beacons firing. **Say:** "this panel is the page's own network traffic — you
can watch the auction outcome, the price, and the audience the request ran
with, per slot."

Consent beat (worth 15s): open a second site, hit **Contextual only** — ads
still serve, but no 👥 chip: no consent → no audience data leaves the platform.

## Scene 3 — advertiser portal (~2 min)

http://localhost:8080/portal/advertiser → `adv-acme@adtech.local`.

1. **Dashboard/campaigns** — spend/impressions ticking (that's the `make
   traffic` terminal feeding it live).
2. **Trace Explorer** — pick a recent impression → the timeline:
   `Audience resolved: <segments> → sent to DSPs` → `You won the auction
   (clearing $X CPM)` → `Impression recorded` → `Viewable (IAB)`. **Say:**
   "any impression, reconstructed end-to-end from the analytics store —
   the platform's whole thesis is zero-slippage traceability."
3. **Bid shading** — the savings view: what the DSP would have paid vs paid.
4. (Optional 20s) staff portal → **Architecture** tab: C4 Containers, click
   the purple DSP box → drills into its components. Nice "how it's built" beat.

## Scene 4 — the shop: retargeting + dynamic product ads (~2 min)

http://shop.adtech.local (external advertiser site with the real pixel):

1. Accept consent, browse a product → the trace panel shows the **RT
   (retargeting pixel)** fire with the SKU.
2. **Add to cart**, then open chronicle in another tab → the chase ad shows
   **the actual carted product** (render-time dynamic product ad). **Say:**
   "enrolled into the retargeting segment in seconds — real-time path, not the
   hourly batch — and the creative is assembled at render time from my cart."
3. Back to the shop → **purchase** → the signed conversion postback fires
   (panel: CONV).
4. Chronicle again → the ad has **rotated to a complementary product** —
   purchase suppression + cross-sell.
5. Advertiser portal → conversions/attribution: the purchase is attributed to
   the exposure chain (click-through/view-through, multi-touch).

## Scene 5 — first-party data finale (~1.5 min) — FLOW VERIFIED LIVE 2026-10-04

The CRM onboarding story — "bring your own customers". The seeded segment
"Diamond & Jewelry Intenders" is **dsp_private**: the list NEVER rides the bid
request (no 👥 trace chip — that's the privacy design and the talking point);
the proof is the pre-wired "High-Intent Luxury" campaign ($40 base bid)
WINNING the moment the list lands. Upload binds by name into the SAME segment
id, so the campaign's targeting is already wired — no targeting step needed.

**Pre-flight for this scene** (makes the upload live-on-camera; the seed
pre-loads the list otherwise):

```sh
kubectl -n adtech exec postgres-0 -- psql -U adtech -d adtech -c \
  "DELETE FROM audience_segment_members WHERE segment_id='d9d0cc1d-9a28-5072-9f1f-83de4c63d4e7';"
# identity resolution must be ON for the internal DSP (how a page visitor
# resolves to the hashed-email member; flipped 2026-10-04, survives reseed
# unless the row is reset to default):
kubectl -n adtech exec postgres-0 -- psql -U adtech -d adtech -c \
  "UPDATE config SET value='\"true\"' WHERE key='dsp.identity_resolution_enabled' AND pod_id='dsp-internal-0';"
```

1. **BEFORE shot** (terminal): auction as the linked persona — a random
   campaign wins at ~$9:
   ```sh
   curl -s "http://localhost:8084/v1/ssp/request?placement_id=pl-sim-mpu&geo=USA&device=desktop&user_id=pid-3efd27156f5ed4b7-2bb968a2-910a6cd5" | python3 -m json.tool | head -20
   ```
2. Advertiser portal (`lumi-re-diamonds@adtech.local` / `admin`) →
   **Audiences** → "Diamond & Jewelry Intenders" shows **0 members** → upload
   `profiles/audiences/diamond-intenders.csv`. Response: 10 added, 1 matched
   (match rate = fraction resolvable via the identity graph). **Say:** "hashed
   ids only — the platform never sees raw emails."
3. **AFTER shot** (~5s later, same curl): **Lumière wins at $60 CPM** —
   the $40-base campaign crushes the ~$9 field because the visitor now
   resolves, through the identity graph, to a member of my private list.
4. **Say:** "my customer list never touched the bid request — no other bidder
   ever saw it — but my campaign just outbid everyone for exactly the people
   on it. Onboarding to winning, in seconds, end to end on this machine."

Close on the Trace Explorer timeline for that win — the one-request story is
the mic drop (the win shows the campaign + price; the ABSENCE of the private
segment in the audience chip is the privacy point).

## If something looks dead mid-recording

`make stack-doctor` first. Demo sites not filling on ONE site = household
frequency caps from your own repeated views (switch site or `make reset &&
make demo`). Panel shows no audience chip = you clicked "Contextual only" on
that site earlier (clear cookies for that origin).
