// The cross-site "chase me" retargeting demo, driven by a real browser.
// Automates docs/demos/RETARGETING-CHASE-DEMO.md end to end:
//
//   1. one persona (random per run) browses the coffee blog → baseline ad,
//      never the chase ad;
//   2. the same persona opens the shop's checkout → retargeting pixel fires
//      (asserted via the shop's pixel overlay + the Postgres enrollment row);
//   3. back on the coffee blog, the chase campaign takes the slot — via the
//      hashed-email identity bridge, since the publisher-side id never
//      touched the shop (this leg waits out the DSP's ≤5m identity-snapshot
//      refresh, so the test budget is long);
//   4. the persona buys → purchase suppression removes the enrollment
//      (asserted in Postgres — the ground truth; the slot itself goes
//      ambiguous under frequency caps).
//
// Prerequisites (the demo doc's setup): seeded stack, and
// dsp.identity_resolution_enabled=true + dsp-internal RESTARTED (the key is
// boot-latched). The spec spawns demosite/demoadv itself.
//
// DO NOT run during a load test (shared cores — see portal-visibility.spec).

const { test, expect } = require('@playwright/test');
const { spawn, execSync } = require('child_process');
const path = require('path');

const REPO = path.resolve(__dirname, '../..');
const COFFEE = 'http://localhost:9300';
const SHOP = 'http://localhost:9400';

function psql(sql) {
  return execSync(
    `kubectl -n adtech exec postgres-0 -- psql -U adtech -d adtech -tA -c "${sql.replace(/"/g, '\\"')}"`,
    { encoding: 'utf8' },
  ).trim();
}

let sites = [];
let chaseCampaignId, barkboxId;

async function waitHealthy(url, ms) {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    try {
      const r = await fetch(url + '/healthz');
      if (r.ok) return;
    } catch { /* not up yet */ }
    await new Promise((r) => setTimeout(r, 500));
  }
  throw new Error(`${url} never became healthy`);
}

test.beforeAll(async () => {
  test.setTimeout(240_000); // compiling both demo sites can take ~1-2 min cold
  barkboxId = psql("SELECT id FROM accounts WHERE name='Premium Dog Food Co'");
  chaseCampaignId = psql("SELECT id FROM line_items WHERE name='Premium Dog Food Co - Cart Retargeting'");
  expect(barkboxId).toMatch(/^[0-9a-f-]{36}$/);
  expect(chaseCampaignId).toMatch(/^[0-9a-f-]{36}$/);

  // A stale site on the port answers /healthz and serves OLD templates.
  // Kill leftovers BY NAME — never by port: ports 9000/9200 belong to the
  // CLUSTER's host-forwards (9000 = Minio), and an `lsof -ti :9000 | kill`
  // assassinates Rancher Desktop's port-forward agent, taking the k8s API
  // (127.0.0.1:6443) down with it — that outage cost three spec runs. The
  // demo sites therefore also run on 9300/9400, which nothing forwards.
  try { execSync('pkill -9 -f chase-demosite; pkill -9 -f chase-demoadv', { stdio: 'ignore' }); } catch { /* none */ }
  execSync('go build -o /tmp/chase-demosite ./cmd/demosite && go build -o /tmp/chase-demoadv ./cmd/demoadv', {
    cwd: REPO, stdio: 'ignore',
  });
  const spawnSite = (bin, env) =>
    sites.push(spawn(bin, [], { cwd: REPO, env: { ...process.env, ...env }, stdio: 'ignore' }));
  spawnSite('/tmp/chase-demosite', {
    DEMOSITE_PORT: '9300',
    DEMOSITE_PUBLISHER_ID: 'pub-third-wave-times',
    DEMOSITE_DISPLAY_PLACEMENT: 'pl-coffee-mpu',
  });
  spawnSite('/tmp/chase-demoadv', {
    DEMOADV_PORT: '9400',
    DEMOADV_ACCOUNT_ID: barkboxId,
    DEMOADV_BRAND: 'Premium Dog Food Co',
    DEMOADV_PRODUCT: '12kg Grain-Free Bag',
  });
  await waitHealthy(COFFEE, 60_000);
  await waitHealthy(SHOP, 60_000);
});

test.afterAll(() => {
  for (const p of sites) p.kill('SIGKILL');
});

test('cart open → chased on the coffee blog → purchase → released', async ({ page }) => {
  test.setTimeout(600_000); // the bridge leg legitimately waits ≤5m (identity snapshot)

  // One fresh persona per run, same email on BOTH origins = one person.
  const persona = `chase-spec-${Math.random().toString(16).slice(2, 8)}@example.com`;
  await page.addInitScript((email) => localStorage.setItem('demo_persona_email', email), persona);

  // ---- 1. Baseline on the coffee blog: consent, ad renders, NOT the chase ad.
  await page.goto(COFFEE + '/');
  await page.click('button.accept');
  await expect(page.locator('#ad-slot-mpu')).not.toContainText('Loading ad', { timeout: 20_000 });
  expect(await page.locator('#ad-slot-mpu').innerHTML()).not.toContain(chaseCampaignId);

  // ---- 2. Open the cart on the shop — first as a GUEST (captured into the
  // audience but NOT bridgeable), then hit the email moment: saving the
  // checkout email hashes it in-browser and re-fires the pixel with the hash,
  // which is what makes the cross-site chase possible at all.
  await page.goto(SHOP + '/checkout');
  await page.click('.consent button.accept');
  await expect(page.locator('#adxlog')).toContainText('GUEST — no email', { timeout: 15_000 });
  await expect(page.locator('#guest-email')).toHaveValue(persona); // persona-bar prefill
  await page.click('#save-email');
  await expect(page.locator('#adxlog')).toContainText('EMAIL CAPTURED', { timeout: 15_000 });

  // Ground truth: audience-rt enrolled this visitor within seconds.
  await expect
    .poll(
      () =>
        Number(
          psql(`SELECT count(*) FROM audience_segment_members m
                JOIN audience_segments s ON s.id=m.segment_id
                WHERE s.name='Shop Checkout Abandoners' AND m.added_at > now() - INTERVAL '2 minutes'`),
        ),
      { timeout: 30_000, message: 'audience-rt never enrolled the shop visitor' },
    )
    .toBeGreaterThan(0);

  // ---- 3. The chase: poll the coffee blog until the cart campaign takes the
  // slot. The publisher-side id never touched the shop — this only works via
  // the hashed-email identity bridge, which activates when the DSP's identity
  // snapshot next refreshes (≤5m).
  let chased = false;
  const deadline = Date.now() + 420_000;
  while (Date.now() < deadline) {
    await page.goto(COFFEE + '/');
    await page.locator('#ad-slot-mpu').waitFor();
    await page.waitForTimeout(2_500); // let the slot fetch + render
    if ((await page.locator('#ad-slot-mpu').innerHTML()).includes(chaseCampaignId)) {
      chased = true;
      break;
    }
    await page.waitForTimeout(10_000);
  }
  expect(chased, 'the chase ad never reached the coffee blog (identity bridge + segment + argmax bid)').toBe(true);

  // ---- 4. Buy → suppression releases the CONVERTING id (Postgres is the
  // ground truth; the slot goes ambiguous under frequency caps).
  //
  // Deliberately id-scoped: suppression as built is ID-level, while the
  // hourly profile-builder enrolls PERSON-level (cluster-expanding to every
  // linked id) and re-qualifies from the still-live site_visit signals on
  // its next pass. Asserting "zero retargeting rows for the advertiser"
  // fails whenever the conductor's :10 run straddles the test — which is
  // exactly how this spec DISCOVERED that suppression is neither
  // person-level nor durable (2026-08-09; a recorded product decision for
  // the operator — see docs/demos/RETARGETING-CHASE-DEMO.md).
  await page.goto(SHOP + '/checkout');
  const buyerId = await page.evaluate(() => localStorage.getItem('adtechadv_uid'));
  expect(buyerId).toBeTruthy();
  await page.click('button.btn.alt');
  await expect(page.locator('#adxlog')).toContainText('CONVERSION recorded', { timeout: 15_000 });
  await expect
    .poll(
      () =>
        Number(
          psql(`SELECT count(*) FROM audience_segment_members m
                JOIN audience_segments s ON s.id=m.segment_id
                WHERE s.account_id='${barkboxId}' AND s.type='retargeting'
                  AND m.user_id='${buyerId}'`),
        ),
      { timeout: 60_000, message: 'purchase suppression never removed the converting id' },
    )
    .toBe(0);
});

// The ANONYMOUS variant: no email is ever typed, no identity bridge exists —
// the chase arrives via the HOUSEHOLD. The shop pixel carries the persona's
// demo IP (derived from the persona email's hash CLIENT-SIDE — the email
// itself never leaves the browser), the tracker enrolls the hh: household id
// alongside the visitor id, and the coffee blog — which derives the SAME
// demo IP for the same persona — gets the chase through the DSP's
// household-keyed lookup. Fast: no identity-snapshot wait (≤~30s drain+serve).
test('anonymous guest cart → household chase on the coffee blog (no email ever)', async ({ page }) => {
  test.setTimeout(180_000);

  const persona = `hh-spec-${Math.random().toString(16).slice(2, 8)}@example.com`;
  await page.addInitScript((email) => localStorage.setItem('demo_persona_email', email), persona);

  // Open the cart as a pure guest — consent yes, email NEVER saved.
  await page.goto(SHOP + '/checkout');
  await page.click('.consent button.accept');
  await expect(page.locator('#adxlog')).toContainText('GUEST — no email', { timeout: 15_000 });

  // Ground truth: the visit enrolled a HOUSEHOLD (hh:) member.
  await expect
    .poll(
      () =>
        Number(
          psql(`SELECT count(*) FROM audience_segment_members m
                JOIN audience_segments s ON s.id=m.segment_id
                WHERE s.name='Shop Checkout Abandoners' AND m.user_id LIKE 'hh:%'
                  AND m.added_at > now() - INTERVAL '90 seconds'`),
        ),
      { timeout: 30_000, message: 'guest cart visit never enrolled a household' },
    )
    .toBeGreaterThan(0);

  // The coffee blog chases the HOUSEHOLD: same persona → same derived demo
  // IP → same hh: id at the SSP → DSP household lookup matches.
  let chased = false;
  const deadline = Date.now() + 90_000;
  while (Date.now() < deadline) {
    await page.goto(COFFEE + '/');
    // First visit on this origin shows the consent banner once.
    const consent = page.locator('button.accept');
    if (await consent.isVisible().catch(() => false)) await consent.click();
    await page.locator('#ad-slot-mpu').waitFor();
    await page.waitForTimeout(2_500);
    if ((await page.locator('#ad-slot-mpu').innerHTML()).includes(chaseCampaignId)) {
      chased = true;
      break;
    }
    await page.waitForTimeout(5_000);
  }
  expect(chased, 'the household chase never reached the coffee blog (hh enrollment → Redis → DSP household lookup)').toBe(true);
});
