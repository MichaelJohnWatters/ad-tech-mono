// Portal visibility suite config. Targets the local gateway; override with
// GATEWAY_URL for a tunnelled/staging stack.
const { defineConfig } = require('@playwright/test');

module.exports = defineConfig({
  testDir: '.',
  timeout: 60_000,
  retries: 0,
  workers: 1, // portals share one gateway; serial keeps act-as cookies isolated
  use: {
    baseURL: process.env.GATEWAY_URL || 'http://localhost:8080',
    screenshot: 'only-on-failure',
    trace: 'retain-on-failure',
  },
  reporter: [['list']],
});
