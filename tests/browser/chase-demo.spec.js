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
const COFFEE = 'http://localhost:9000';
const SHOP = 'http://localhost:9200';

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
  test.setTimeout(240_000); // `go run` cold-compiles both demo sites (~1-2 min)
  barkboxId = psql("SELECT id FROM accounts WHERE name='Premium Dog Food Co'");
  chaseCampaignId = psql("SELECT id FROM line_items WHERE name='Premium Dog Food Co - Cart Retargeting'");
  expect(barkboxId).toMatch(/^[0-9a-f-]{36}$/);
  expect(chaseCampaignId).toMatch(/^[0-9a-f-]{36}$/);

  const spawnSite = (cmd, env) =>
    sites.push(spawn('go', ['run', cmd], { cwd: REPO, env: { ...process.env, ...env }, stdio: 'ignore' }));
  spawnSite('./cmd/demosite', {
    DEMOSITE_PUBLISHER_ID: 'pub-third-wave-times',
    DEMOSITE_DISPLAY_PLACEMENT: 'pl-coffee-mpu',
  });
  spawnSite('./cmd/demoadv', {
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

  // ---- 2. Open the cart on the shop as the same persona.
  await page.goto(SHOP + '/checkout');
  await page.click('.consent button.accept');
  await expect(page.locator('#adxlog')).toContainText('retargeting pixel fired', { timeout: 15_000 });

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

  // ---- 4. Buy → suppression releases the persona (Postgres is the ground
  // truth; the slot goes ambiguous under frequency caps).
  await page.goto(SHOP + '/checkout');
  await page.click('button.btn.alt');
  await expect(page.locator('#adxlog')).toContainText('CONVERSION recorded', { timeout: 15_000 });
  await expect
    .poll(
      () =>
        Number(
          psql(`SELECT count(*) FROM audience_segment_members m
                JOIN audience_segments s ON s.id=m.segment_id
                WHERE s.account_id='${barkboxId}' AND s.type='retargeting'
                  AND m.added_at > now() - INTERVAL '10 minutes'`),
        ),
      { timeout: 60_000, message: 'purchase suppression never removed the enrollment' },
    )
    .toBe(0);
});
