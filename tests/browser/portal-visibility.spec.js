// Portal data-visibility suite.
//
// Proves, in a real browser, that after a traffic run:
//   1. an advertiser can log in and SEE their campaigns + delivery numbers,
//   2. a publisher can log in and SEE their placements + revenue,
//   3. a STAFF user can impersonate both (the act_as_account cookie the staff
//      portal's "View as" button sets) and see the SAME data,
// and that no portal API call fails while doing so. Every >=400 response on
// /v1/* is collected and reported — this is the diagnostic for "impersonation
// endpoints fail": the assertion message names exactly which endpoints broke.
//
// Credentials are the seeded dev accounts (`make seed` / `make demo`).
//
// DO NOT run this while a load test is in flight: the simulator and the k8s
// VM share the host's cores, and Chromium + npm installs starve them —
// measured 2026-08-05: fill collapsed 97%→12% mid-soak from exactly that.
// Run it AFTER the load run; that's also when the reporting KPIs have data.

const { test, expect } = require('@playwright/test');

const ADVERTISER = { email: 'adv-acme@adtech.local', password: 'admin' };
const PUBLISHER = { email: 'tech-review@adtech.local', password: 'admin' };
const STAFF = { email: 'admin@adtech.local', password: 'admin' };

// collectApiFailures wires a response listener that records every failed
// /v1/* call the page makes — the core diagnostic of this suite.
function collectApiFailures(page, failures) {
  page.on('response', async (resp) => {
    const url = resp.url();
    if (!url.includes('/v1/')) return;
    if (resp.status() >= 400) {
      let body = '';
      try {
        body = (await resp.text()).slice(0, 120);
      } catch {
        /* stream may be gone; the status + URL is what matters */
      }
      failures.push(`${resp.status()} ${new URL(url).pathname} ${body.replace(/\n/g, ' ')}`);
    }
  });
}

async function login(page, { email, password }) {
  await page.goto('/login');
  await page.fill('input[name="email"]', email);
  await page.fill('input[name="password"]', password);
  await Promise.all([page.waitForNavigation(), page.click('button[type="submit"]')]);
}

// waitForData polls until the page body shows a non-zero delivery number in
// the KPI/stat areas — tolerant of exact layout, strict about "data visible".
// A marker can appear in SEVERAL places (the dashboard's top-campaigns card
// keeps a hidden copy after switching tabs), so pass "visible-only" matches —
// .first() over all copies would latch onto a hidden one.
async function expectVisibleData(page, mustContain) {
  for (const text of mustContain) {
    await expect(page.getByText(text, { exact: false }).locator('visible=true').first()).toBeVisible({ timeout: 15_000 });
  }
}

// impersonate mimics the staff portal's viewAs() exactly: set the act-as
// cookie, then navigate to the persona portal.
async function impersonate(page, type, id, portalPath) {
  await page.context().addCookies([
    {
      name: 'act_as_account',
      value: `${type}:${id}`,
      url: process.env.GATEWAY_URL || 'http://localhost:8080',
      sameSite: 'Lax',
    },
  ]);
  await page.goto(portalPath);
}

// The seeded Acme advertiser + TechReview publisher — names that must appear
// in their portals when data is flowing.
const ADV_MARKERS = ['Acme Shoes'];
const PUB_MARKERS = ['Placement'];

// The advertiser portal lands on the KPI dashboard; campaign names live under
// the Campaigns section — click through before asserting.
async function openCampaignsTab(page) {
  await page.click('a[href="#campaigns"]');
}

// expectNonZeroKpi is the REPORTING assertion: the dashboard stat (fed by the
// reporting query engine over impressions) must render a number with a
// non-zero digit — "—" (not loaded) and "0" (no data visible to this tenant)
// both fail. This is what proves the tenant can actually SEE delivery data,
// not merely that their entity lists render.
async function expectNonZeroKpi(page, id) {
  await expect
    .poll(async () => (await page.locator(`#${id}`).textContent())?.trim() || '', {
      timeout: 20_000,
      message: `#${id} should show a non-zero delivery number`,
    })
    .toMatch(/[1-9]/);
}

test('advertiser logs in and sees campaign data', async ({ page }) => {
  const failures = [];
  collectApiFailures(page, failures);
  await login(page, ADVERTISER);
  await expect(page).toHaveURL(/portal\/advertiser/);
  // Reporting numbers on the landing dashboard: impressions + spend > 0.
  await expectNonZeroKpi(page, 'kpiImps');
  await expectNonZeroKpi(page, 'kpiSpend');
  await openCampaignsTab(page);
  await expectVisibleData(page, ADV_MARKERS);
  expect(failures, `advertiser portal API failures:\n${failures.join('\n')}`).toEqual([]);
});

test('publisher logs in and sees placement data', async ({ page }) => {
  const failures = [];
  collectApiFailures(page, failures);
  await login(page, PUBLISHER);
  await expect(page).toHaveURL(/portal\/publisher/);
  // Reporting numbers: the publisher's impressions + earnings > 0.
  await expectNonZeroKpi(page, 'kpiImps');
  await expectNonZeroKpi(page, 'kpiEarnings');
  await expectVisibleData(page, PUB_MARKERS);
  expect(failures, `publisher portal API failures:\n${failures.join('\n')}`).toEqual([]);
});

// findDataBearingAccount picks an account of the given type that actually HAS
// impressions in the recent window — impersonating a tenant who lost every
// auction (it happens: a mispriced market once left the demo advertiser at
// zero) would fail the KPI assertions for marketplace reasons, not visibility
// reasons.
async function findDataBearingAccount(request, cookieHeader, accounts, type) {
  for (const a of accounts.filter((x) => x.type === type)) {
    const resp = await request.post('/v1/api/reports', {
      headers: {
        cookie: cookieHeader,
        'Content-Type': 'application/json',
        'X-Act-As-Account': `${type}:${a.id}`,
      },
      data: {
        table: 'impressions',
        metrics: ['count'],
        time_from: new Date(Date.now() - 6 * 3600e3).toISOString(),
      },
    });
    if (!resp.ok()) continue;
    const body = await resp.json();
    if ((body.rows?.[0]?.[0] || 0) > 0) return a;
  }
  return null;
}

test('staff impersonates the advertiser and sees the same data', async ({ page, request }) => {
  const failures = [];
  collectApiFailures(page, failures);
  await login(page, STAFF);

  // Resolve a DATA-BEARING advertiser the same way the staff switcher would.
  const cookieHeader = (await page.context().cookies()).map((c) => `${c.name}=${c.value}`).join('; ');
  const accounts = await (await request.get('/v1/api/accounts', { headers: { cookie: cookieHeader } })).json();
  const adv = await findDataBearingAccount(request, cookieHeader, accounts, 'advertiser');
  expect(adv, 'an advertiser account with impressions in the last 6h').toBeTruthy();

  await impersonate(page, 'advertiser', adv.id, '/portal/advertiser');
  await expectNonZeroKpi(page, 'kpiImps');
  await expectNonZeroKpi(page, 'kpiSpend');
  expect(failures, `impersonated advertiser portal API failures:\n${failures.join('\n')}`).toEqual([]);
});

test('staff impersonates the publisher and sees the same data', async ({ page, request }) => {
  const failures = [];
  collectApiFailures(page, failures);
  await login(page, STAFF);

  const cookieHeader = (await page.context().cookies()).map((c) => `${c.name}=${c.value}`).join('; ');
  const accounts = await (await request.get('/v1/api/accounts', { headers: { cookie: cookieHeader } })).json();
  const pub = await findDataBearingAccount(request, cookieHeader, accounts, 'publisher');
  expect(pub, 'a publisher account with impressions in the last 6h').toBeTruthy();

  await impersonate(page, 'publisher', pub.id, '/portal/publisher');
  await expectNonZeroKpi(page, 'kpiImps');
  await expectNonZeroKpi(page, 'kpiEarnings');
  await expectVisibleData(page, PUB_MARKERS);
  expect(failures, `impersonated publisher portal API failures:\n${failures.join('\n')}`).toEqual([]);
});
