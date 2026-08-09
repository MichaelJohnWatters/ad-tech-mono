# The "chase me" demo — open a cart, get followed, buy, get released

A two-tab, real-browser demo of cross-site retargeting: you (a human, no
simulator) browse a coffee blog and see a normal ad; you open a cart on a
shop site; within moments the shop's ad is chasing you around the coffee
blog; you buy, and it stops. Every hop is the real production path —
first-party ids, hashed-email identity bridge, real-time enrollment,
auction targeting, purchase suppression.

## One-time setup (per stack)

```bash
# 1. The seeded world already contains the audience + chase campaign
#    ("Shop Checkout Abandoners" + "Premium Dog Food Co - Cart Retargeting").
#    Just make sure the stack is seeded:
make seed

# 2. Turn on DSP identity resolution (off by default — it's the cross-site
#    bridge) and RESTART dsp-internal: the key is TierLive but the resolver
#    is constructed at BOOT (openIdentityResolver returns nil when disabled
#    at startup), so a live flip alone does nothing — found the hard way
#    wiring this demo.
kubectl -n adtech exec postgres-0 -- psql -U adtech -d adtech -c \
  "UPDATE config SET value='\"true\"'::jsonb WHERE key='dsp.identity_resolution_enabled' AND pod_id LIKE 'dsp-internal%'"
kubectl -n adtech rollout restart deploy/dsp-internal
```

## Start the two demo sites (two terminals)

```bash
# The PUBLISHER — run the demo news site as the coffee blog "Third Wave Times":
DEMOSITE_PUBLISHER_ID=pub-third-wave-times \
DEMOSITE_DISPLAY_PLACEMENT=pl-coffee-mpu \
go run ./cmd/demosite          # → http://localhost:9000

# The SHOP — run the demo advertiser as Premium Dog Food Co
# (account id = the seeded adv-barkbox uuid; grab it once):
BARKBOX=$(kubectl -n adtech exec postgres-0 -- psql -U adtech -d adtech -tA -c \
  "SELECT id FROM accounts WHERE name='Premium Dog Food Co'")
DEMOADV_ACCOUNT_ID=$BARKBOX \
DEMOADV_BRAND="Premium Dog Food Co" \
DEMOADV_PRODUCT="12kg Grain-Free Bag" \
go run ./cmd/demoadv           # → http://localhost:9200
```

## The script

1. **Be one person on both sites.** Each site shows a persona bar
   (bottom-left) with your demo email. Click **New visitor** on the coffee
   blog, then **Change** on the shop and enter the SAME email. That email —
   hashed in the browser, never sent raw — is how the platform will link
   your two first-party ids.
2. **Baseline.** On the coffee blog, accept personalized ads. The slot
   shows whatever normally wins (a coffee ad if you browse a while, or a
   market campaign). Refresh a couple of times — no dog food anywhere.
3. **Open the cart.** On the shop, accept the consent banner, browse to the
   product, then **Checkout**. Watch the pixel overlay (bottom-right):
   `retargeting pixel fired (+hashed email) · tag="checkout"`. At this
   moment audience-rt enrolls your shop-visitor id into "Shop Checkout
   Abandoners" (check the advertiser portal → Audiences to see yourself
   counted), and the changelog drainer has it in Redis ~3s later.
4. **The chase.** Back on the coffee blog, refresh. The DSP resolves your
   publisher-side id → hashed email → shop-visitor id, finds the cart
   segment, and the "Cart Retargeting" campaign (deliberately the highest
   bid in the market) takes the slot. **Timing**: the identity link rides
   the DSP's in-memory graph snapshot, refreshed every 5 minutes — so the
   chase starts anywhere from instantly to ~5 min after the cart visit.
   Narrate the portal while you wait, or pre-warm by doing step 3 first.
5. **Buy your freedom.** On the shop's checkout, complete the order. The
   overlay logs the SIGNED server-to-server conversion; audience-rt sees
   the purchase and removes you from the cart segments within seconds.
   Refresh the coffee blog — the chase is gone.
6. **Reset.** Click **New visitor** on both sites (fresh email + fresh
   first-party ids = a genuinely new person; your old enrollment ages out
   on its own via the 30-day TTL). Or demo the GDPR path instead and be
   forgotten properly.

## What to show while narrating

- Advertiser portal (`premium-dog-food-co@adtech.local` / `admin`) →
  Audiences: live enrollment counts on the cart segments.
- Staff portal → Trace explorer: the serve trace showing the DSP's
  segment match.
- `docs/AUDIENCE-PIPELINE.md` for the architecture behind each hop.

## Gotchas

- **Frequency caps are real**: the same user/household stops being served
  the same campaign after a few impressions — refreshing twenty times
  makes ads *disappear*, which is capping working, not the demo breaking.
  Move on, or become a New visitor.
- **Households are per-IP, and locally everything shares one IP** — so the
  demosite persona bar also assigns each persona its own demo household
  (a random TEST-NET address sent via the allowlist-gated `?ip=` override;
  ignored from public browsers in prod). "New visitor" therefore resets
  the household too. Without it, every visitor on your laptop would share
  one household's frequency caps forever.
- The persona email must match on BOTH sites or there is no bridge — the
  persona bars exist precisely because the old baked-in shared email made
  every visitor the same person forever.
- The chase campaign bids 22 CPM on purpose: the internal DSP submits its
  single BEST eligible campaign per request, so the chase must top the
  big-world premium stratum (19.5) to surface on density-seeded worlds.
- Consent matters end-to-end: decline on either site and that side goes
  contextual-only (also demoable — the pixel overlay says so).
